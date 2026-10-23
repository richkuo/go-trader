package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func pairEnterExitRunner(e *pairTestEnv, hlQty float64) {
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
		if req.Lifecycle == pairLifecycleMatched {
			return pairExitResult(req, hlQty > 0), nil
		}
		return pairEnterResult(req, hlQty), nil
	}
}

func pairEnterHoldRunner(e *pairTestEnv, hlQty float64) {
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
		if req.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(req, "keep the spread"), nil
		}
		return pairEnterResult(req, hlQty), nil
	}
}

func pairSlotAt(t0 time.Time, i int) time.Time {
	return t0.Add(time.Duration(i) * time.Minute)
}

func (e *pairTestEnv) slotStep(i int) time.Time {
	at := pairSlotAt(pairTestStart(), i)
	e.slot(at, fmt.Sprintf("in-%d", i))
	e.step(at.Add(time.Second))
	return at
}

func (e *pairTestEnv) assertBooks(label string, hl, okx float64) {
	e.t.Helper()
	if got := e.signed(pairTestHL); !basketQtyEqual(got, hl) {
		e.t.Fatalf("%s: hyperliquid member holds %.10g, want %.10g", label, got, hl)
	}
	if got := e.signed(pairTestOKX); !basketQtyEqual(got, okx) {
		e.t.Fatalf("%s: okx member holds %.10g, want %.10g", label, got, okx)
	}
}

func (e *pairTestEnv) lifecycle() string {
	return e.rt().Pair.Progress.Lifecycle
}

func TestBasketPairEntryHedgeExitLifecycle(t *testing.T) {
	for _, hlQty := range []float64{0.06, -0.06} {
		t.Run(fmt.Sprintf("hl_%v", hlQty), func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterExitRunner(e, hlQty)
			e.slotStep(0)
			e.assertBooks("first leg", 0, -hlQty)
			if e.lifecycle() != pairLifecycleOpening {
				t.Fatalf("lifecycle after the first leg is %s", e.lifecycle())
			}
			if n := e.count(`SELECT COUNT(*) FROM pair_quote_event_uses WHERE status = 'consumed' AND strategy_id = ?`, pairTestOKX); n != 1 {
				t.Fatalf("the first leg consumed %d quote events, want 1", n)
			}
			e.slotStep(1)
			e.assertBooks("hedged", hlQty, -hlQty)
			if e.lifecycle() != pairLifecycleMatched {
				t.Fatalf("lifecycle after the hedge is %s", e.lifecycle())
			}
			if e.rt().Pair.Progress.FirstFillMs != pairSlotAt(pairTestStart(), 0).Add(time.Second).UnixMilli() {
				t.Fatalf("the first fill time is %d", e.rt().Pair.Progress.FirstFillMs)
			}
			e.slotStep(2)
			e.assertBooks("exited", 0, 0)
			e.slotStep(3)
			if e.lifecycle() != pairLifecycleOpening && e.lifecycle() != pairLifecycleFlat {
				t.Fatalf("lifecycle after the exit is %s", e.lifecycle())
			}
			if n := e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair' AND status IN ('reserved','submitting','unresolved','settling')`); n != 0 {
				t.Fatalf("%d pair actions stayed unresolved after the round trip", n)
			}
			if n := e.count(`SELECT COUNT(*) FROM entry_order_intents WHERE owner_mode = 'pair' AND decision_kind = 'quote' AND interval_ms = 0 AND status = 'committed'`); n < 2 {
				t.Fatalf("the linked quote-event entry intents committed %d times, want at least 2", n)
			}
		})
	}
}

func TestBasketPairDecisionEntryPointOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		respond func(req pairPolicyRequest) (pairPolicyResult, error)
		raw     func(req pairPolicyRequest) []byte
		nilRun  bool
		status  string
		kind    string
	}{
		{name: "enter", respond: func(r pairPolicyRequest) (pairPolicyResult, error) { return pairEnterResult(r, 0.05), nil }, status: basketRebalanceExecuting, kind: pairPolicyStatusEnter},
		{name: "hold", respond: func(r pairPolicyRequest) (pairPolicyResult, error) {
			return pairHoldResult(r, "spread inside costs"), nil
		}, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "invalid", respond: func(r pairPolicyRequest) (pairPolicyResult, error) {
			res := pairHoldResult(r, "")
			res.Status, res.Error = pairPolicyStatusInvalid, "quotes crossed"
			return res, nil
		}, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "wrong identity", respond: func(r pairPolicyRequest) (pairPolicyResult, error) {
			res := pairEnterResult(r, 0.05)
			res.InputFingerprint = "other"
			return res, nil
		}, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "exit while flat", respond: func(r pairPolicyRequest) (pairPolicyResult, error) { return pairExitResult(r, true), nil }, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "unequal legs", respond: func(r pairPolicyRequest) (pairPolicyResult, error) {
			res := pairEnterResult(r, 0.05)
			t := res.Targets[pairTestOKX]
			t.TargetBaseQty = -0.04
			res.Targets[pairTestOKX] = t
			return res, nil
		}, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "illegal lot", respond: func(r pairPolicyRequest) (pairPolicyResult, error) { return pairEnterResult(r, 0.055), nil }, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "unreadable", raw: func(pairPolicyRequest) []byte { return []byte("{not json") }, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "runner error", respond: func(pairPolicyRequest) (pairPolicyResult, error) {
			return pairPolicyResult{}, errors.New("policy process exited 1")
		}, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "nil runner", nilRun: true, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
		{name: "cap at full fill", respond: func(r pairPolicyRequest) (pairPolicyResult, error) { return pairEnterResult(r, 0.12), nil }, status: basketRebalanceHeld, kind: pairPolicyStatusHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			e.runner.respond, e.runner.raw = tc.respond, tc.raw
			if tc.nilRun {
				e.bc.pairPolicyRun = nil
			}
			at := pairTestStart()
			e.slot(at, "in-0")
			e.step(at.Add(time.Second))
			rb := e.rt().Rebalances[at.UnixMilli()]
			if rb.Status != tc.status {
				t.Fatalf("slot status %s, want %s (hold %q)", rb.Status, tc.status, rb.HoldReason)
			}
			var stored string
			if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, at.UnixMilli()).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			got, err := parsePairTargets(stored)
			if err != nil || got == nil || got.Kind != tc.kind || got.SlotMs != at.UnixMilli() || got.InputFingerprint != "in-0" {
				t.Fatalf("stored decision %q (%v), want kind %s bound to the slot", stored, err, tc.kind)
			}
			if tc.kind == pairPolicyStatusHold {
				e.assertBooks("held", 0, 0)
				if n := e.count(`SELECT COUNT(*) FROM basket_member_actions`); n != 0 {
					t.Fatalf("a held decision reserved %d actions", n)
				}
			}
		})
	}
}

func TestBasketPairDecisionTimeoutAndChangedSnapshot(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.runner.block = make(chan struct{})
		at := pairTestStart()
		e.slot(at, "in-0")
		e.step(at.Add(60*time.Second - 50*time.Millisecond))
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceHeld {
			t.Fatalf("a policy timeout must hold the slot, got %s", rb.Status)
		}
		close(e.runner.block)
	})
	t.Run("changed snapshot", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		at := pairTestStart()
		e.slot(at, "in-0")
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			e.mu.Lock()
			s := e.state.Strategies[pairTestHL]
			s.Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 0.01, AvgCost: 60000, Multiplier: 1}
			e.mu.Unlock()
			return pairEnterResult(r, 0.05), nil
		}
		e.step(at.Add(time.Second))
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.TargetsJSON != "" {
			t.Fatalf("a result computed on a changed snapshot was frozen: %s", rb.TargetsJSON)
		}
	})
	t.Run("restart between envelope and decision", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		at := pairTestStart()
		e.slot(at, "in-0")
		e.restart()
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb == nil || rb.Status != basketRebalanceFrozen || rb.TargetsJSON != "" {
			t.Fatalf("an undecided frozen slot must survive a restart undecided: %+v", rb)
		}
		pairEnterExitRunner(e, 0.05)
		e.step(at.Add(time.Second))
		if e.rt().Rebalances[at.UnixMilli()].Status != basketRebalanceExecuting {
			t.Fatalf("the restarted slot was not decided from its saved envelope")
		}
		req := e.runner.requests[len(e.runner.requests)-1]
		if req.InputFingerprint != "in-0" || string(req.Envelope) != `{"quotes":"in-0"}` || req.CutoffMs != at.UnixMilli()+500 {
			t.Fatalf("the decision request does not carry the saved envelope: %+v", req)
		}
	})
	t.Run("interrupted unfrozen slot holds", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		at := pairTestStart()
		e.mu.Lock()
		if _, err := e.bc.reservePairSlot(e.rt(), at.UnixMilli(), at); err != nil {
			t.Fatal(err)
		}
		e.mu.Unlock()
		e.restart()
		rb := e.rt().Rebalances[at.UnixMilli()]
		var status string
		if err := e.db().db.QueryRow(`SELECT status FROM basket_rebalances WHERE deadline_ms = ?`, at.UnixMilli()).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if rb.Status != basketRebalanceHeld || status != basketRebalanceHeld {
			t.Fatalf("an interrupted reserved slot must become a durable hold, got %s/%s", rb.Status, status)
		}
	})
	t.Run("non-flat slot is evidence only", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.venues.hl.mu.Lock()
		e.venues.hl.outcomes = []basketVenueOutcome{{Outcome: basketVenueUnknown, Detail: "timeout"}}
		e.venues.hl.mu.Unlock()
		e.slotStep(0)
		e.slotStep(1)
		calls := e.runner.calls
		at := e.slotStep(2)
		if e.runner.calls != calls {
			t.Fatalf("the policy ran while the pair was %s", e.lifecycle())
		}
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceCompleted || rb.Outcome.Result != "evidence_only" {
			t.Fatalf("a slot in the %s lifecycle must end evidence only, got %s/%s", e.lifecycle(), rb.Status, rb.Outcome.Result)
		}
	})
}

func pairOpenVenue(e *pairTestEnv, id string, outs ...basketVenueOutcome) {
	v := e.venues.of(id)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.outcomes = append(v.outcomes, outs...)
}

func pairQueryVenue(e *pairTestEnv, a *basketAction, outs ...basketVenueOutcome) {
	v := e.venues.of(a.StrategyID)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.queries[a.ClientOrderID] = append(v.queries[a.ClientOrderID], outs...)
}

func pairAnswerVenue(e *pairTestEnv, a *basketAction, out basketVenueOutcome) {
	v := e.venues.of(a.StrategyID)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.queries[a.ClientOrderID] = []basketVenueOutcome{out}
}

func (e *pairTestEnv) bookedTrades(id string) int {
	n := 0
	for _, tr := range e.state.Strategies[id].TradeHistory {
		if tr.Symbol == "BTC" && tr.TradeType != TradeTypeFunding {
			n++
		}
	}
	return n
}

func TestBasketPairPartialOpenFillsBookEachDeltaOnce(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800})
	e.slotStep(0)
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 || first[0].Status != basketActionUnresolved || !basketQtyEqual(first[0].AppliedQty, 0.02) {
		t.Fatalf("an open partial fill must book its delta and stay unresolved: %+v", first)
	}
	e.assertBooks("partial", 0, -0.02)
	a := first[0]
	pairQueryVenue(e, a,
		basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800},
		basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.05, CumNotional: 0.02*59800 + 0.03*59750},
		basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.02*59800 + 0.03*59750 + 0.01*59720, Terminal: true},
	)
	for i := 1; i <= 3; i++ {
		at := pairSlotAt(pairTestStart(), i)
		e.step(at)
	}
	e.assertBooks("filled", 0, -0.06)
	if a.Status != basketActionCommitted || !basketQtyEqual(a.AppliedQty, 0.06) {
		t.Fatalf("the filled action must commit with every delta applied: %+v", a)
	}
	if n := e.bookedTrades(pairTestOKX); n != 3 {
		t.Fatalf("three cumulative confirmations must book three trades, got %d", n)
	}
	var lastPx float64
	for _, tr := range e.state.Strategies[pairTestOKX].TradeHistory {
		if tr.Symbol == "BTC" {
			lastPx = tr.Price
		}
	}
	if math.Abs(lastPx-59720) > 1e-6 {
		t.Fatalf("the last incremental fill must book at the price from the notional delta, got %v", lastPx)
	}
	e.restart()
	e.step(pairSlotAt(pairTestStart(), 4))
	e.assertBooks("after restart", 0, -0.06)
	if n := e.bookedTrades(pairTestOKX); n != 3 {
		t.Fatalf("a restart must not book a confirmed delta again, got %d trades", n)
	}
}

func TestBasketPairMalformedAndInconsistentEvidenceHolds(t *testing.T) {
	cases := []struct {
		name string
		out  basketVenueOutcome
	}{
		{"overfill", basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.07, CumNotional: 0.07 * 59800, Terminal: true}},
		{"no notional", basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02}},
		{"price outside bound", basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59000, Terminal: true}},
		{"wrong account", basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true, AccountKey: "synthetic:other"}},
		{"accepted after deadline", basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true, AcceptedAtMs: math.MaxInt32 * 1000}},
		{"unknown outcome kind", basketVenueOutcome{Outcome: "settled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterExitRunner(e, 0.06)
			pairOpenVenue(e, pairTestOKX, tc.out)
			e.slotStep(0)
			a := e.actionsByPurpose(pairPurposeEntry)[0]
			if !basketActionPending(a.Status) || a.AppliedQty != 0 {
				t.Fatalf("bad evidence must keep the action unresolved with nothing booked: %+v", a)
			}
			e.assertBooks("held", 0, 0)
			if !e.rt().Pair.Progress.IncreasesFrozen {
				t.Fatal("bad evidence must freeze increases")
			}
			var status string
			var applied float64
			if err := e.db().db.QueryRow(`SELECT status, applied_qty FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&status, &applied); err != nil {
				t.Fatal(err)
			}
			if !basketActionPending(status) || applied != 0 {
				t.Fatalf("the saved action must stay unresolved with nothing applied, got %s %v", status, applied)
			}
		})
	}
}

func TestBasketPairDecreasingAndConflictingEvidenceIsRefused(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.03, CumNotional: 0.03 * 59800})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 59800})
	e.step(pairSlotAt(pairTestStart(), 1))
	if !basketQtyEqual(a.ConfirmedQty, 0.03) || !basketQtyEqual(a.AppliedQty, 0.03) {
		t.Fatalf("a decreasing confirmation must be refused: %+v", a)
	}
	e.assertBooks("after decrease", 0, -0.03)
	before := a.ConfirmedQty
	e.mu.Lock()
	a.ConfirmedQty = 0.01
	err := e.store.PersistBasketProgress(e.rt())
	a.ConfirmedQty = before
	e.mu.Unlock()
	if !errors.Is(err, errPairGuardMiss) {
		t.Fatalf("a stale runtime save with lower confirmed evidence must fail its transaction, got %v", err)
	}
	var saved float64
	if err := e.db().db.QueryRow(`SELECT confirmed_qty FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if !basketQtyEqual(saved, 0.03) {
		t.Fatalf("the saved confirmation moved to %v", saved)
	}
}

func TestBasketPairDuplicateEventsNeverEnlargeIntent(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	e.assertBooks("first leg", 0, -0.06)
	for i := 0; i < 3; i++ {
		e.step(pairSlotAt(pairTestStart(), 0).Add(time.Duration(i+2) * time.Second))
	}
	e.assertBooks("no fresh event", 0, -0.06)
	e.mu.Lock()
	rt := e.rt()
	ev, _ := pairQuoteEventFromSlot(rt.Rebalances[pairTestStart().UnixMilli()])
	use := &pairQuoteEventUse{Partition: rt.Partition, PairID: pairTestID, StrategyID: pairTestOKX, Event: ev, EventID: ev.ID(), Status: pairUseReserved, CreatedAtMs: 1, UpdatedAtMs: 1}
	db, _ := e.store.basketFile(rt)
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := db.toStorageID(pairTestOKX)
	dupErr := insertPairQuoteUseTx(tx, sid, use)
	tx.Rollback()
	e.mu.Unlock()
	if dupErr == nil {
		t.Fatal("a second use of one quote event by one member must be refused")
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_quote_event_uses WHERE strategy_id = ?`, pairTestOKX); n != 1 {
		t.Fatalf("the member holds %d quote uses, want 1", n)
	}
	e.slotStep(1)
	e.assertBooks("hedged on the fresh event", 0.06, -0.06)
}

func TestBasketPairDeadlineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline int64
		sent     bool
	}{
		{"one millisecond before the deadline sends", 1, true},
		{"at the deadline nothing is sent", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterExitRunner(e, 0.06)
			v := e.venues.okx
			v.mu.Lock()
			v.onSubmit = func(a *basketAction) {}
			v.mu.Unlock()
			sentAt := int64(0)
			e.venues.okx.mu.Lock()
			e.venues.okx.outcomes = nil
			e.venues.okx.mu.Unlock()
			at := pairTestStart()
			e.slot(at, "in-0")
			nowMs := at.Add(time.Second).UnixMilli()
			e.venues.okx.mu.Lock()
			e.venues.okx.onSubmit = func(a *basketAction) { sentAt = a.AcceptDeadlineMs }
			e.venues.okx.mu.Unlock()
			e.mu.Lock()
			e.rt().Pair.Limits.AcceptanceDeadlineMs = tc.deadline
			e.mu.Unlock()
			e.step(time.UnixMilli(nowMs).UTC())
			a := e.actionsByPurpose(pairPurposeEntry)
			if len(a) != 1 {
				t.Fatalf("want one reserved first leg, got %d", len(a))
			}
			if tc.sent {
				if sentAt != nowMs+1 || a[0].Status != basketActionCommitted {
					t.Fatalf("a submit one millisecond before the deadline must send: sent deadline %d action %+v", sentAt, a[0])
				}
				return
			}
			if sentAt != 0 || a[0].Status != basketActionNoFill || !a[0].Terminal {
				t.Fatalf("a submit at the deadline must send nothing and end never sent: sent %d action %+v", sentAt, a[0])
			}
			e.assertBooks("never sent", 0, 0)
		})
	}
}

func TestBasketPairAbsenceReleaseBoundary(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	limit := a.AcceptDeadlineMs + entryRecoveryGrace.Milliseconds()
	absent := basketVenueOutcome{Outcome: pairVenueAbsent, CoverageComplete: true, CoverageThroughMs: limit}
	pairQueryVenue(e, a, absent, absent)
	e.step(time.UnixMilli(limit).UTC())
	if a.Status != basketActionUnresolved {
		t.Fatalf("absence at exactly the deadline plus grace must not release the action, got %s", a.Status)
	}
	e.step(time.UnixMilli(limit + 1).UTC())
	if a.Status != basketActionNoFill || !a.Terminal {
		t.Fatalf("absence one millisecond after the deadline plus grace with full coverage must release, got %s", a.Status)
	}
	var intentStatus string
	if err := e.db().db.QueryRow(`SELECT status FROM entry_order_intents WHERE intent_id = ?`, a.EntryIntentID).Scan(&intentStatus); err != nil {
		t.Fatal(err)
	}
	if intentStatus != entryIntentNoFill {
		t.Fatalf("the released action's entry intent is %s", intentStatus)
	}
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a released first leg must leave the pair flat, got %s", e.lifecycle())
	}
}

func TestBasketPairAbsenceNeedsCoverageAndNoContraryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  func(limit int64) basketVenueOutcome
	}{
		{"short coverage", func(limit int64) basketVenueOutcome {
			return basketVenueOutcome{Outcome: pairVenueAbsent, CoverageComplete: true, CoverageThroughMs: limit - 1}
		}},
		{"incomplete history", func(limit int64) basketVenueOutcome {
			return basketVenueOutcome{Outcome: pairVenueAbsent, CoverageComplete: false, CoverageThroughMs: limit + 10}
		}},
		{"contrary evidence", func(limit int64) basketVenueOutcome {
			return basketVenueOutcome{Outcome: pairVenueAbsent, CoverageComplete: true, ContraryEvidence: true, CoverageThroughMs: limit + 10}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			pairEnterExitRunner(e, 0.06)
			pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
			e.slotStep(0)
			a := e.actionsByPurpose(pairPurposeEntry)[0]
			limit := a.AcceptDeadlineMs + entryRecoveryGrace.Milliseconds()
			pairQueryVenue(e, a, tc.out(limit))
			e.step(time.UnixMilli(limit + 5).UTC())
			if a.Status != basketActionUnresolved {
				t.Fatalf("uncertain absence must not release the action, got %s", a.Status)
			}
		})
	}
}

func TestBasketPairUnknownElectiveNeverBlocksProtectiveWork(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.02, CumNotional: 0.02 * 60200})
	e.slotStep(1)
	e.assertBooks("partial hedge", 0.02, -0.06)
	hedge := e.actionsByPurpose(pairPurposeEntry)[1]
	if hedge.Status != basketActionUnresolved {
		t.Fatalf("the open hedge must stay unresolved: %+v", hedge)
	}
	e.externalBookChange(pairTestOKX, -0.03)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.step(pairSlotAt(pairTestStart(), 2))
	prot := e.actionsByPurpose(pairPurposeProtective)
	if len(prot) != 2 {
		t.Fatalf("each member must reserve its own protective reduction beside the unknown elective action, got %d", len(prot))
	}
	if n := e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair' AND status IN ('reserved','submitting','unresolved','settling')`); n != 3 {
		t.Fatalf("three unresolved pair actions must be saved, got %d", n)
	}
	e.restart()
	if got := len(e.rt().pairUnresolvedActions()); got != 3 {
		t.Fatalf("a restart must recover all three unresolved actions, got %d", got)
	}
	saved := e.actionsByPurpose(pairPurposeProtective)
	for _, p := range saved {
		pairQueryVenue(e, p, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: p.RequestedQty, CumNotional: p.RequestedQty * 60000, Terminal: true})
	}
	e.step(pairSlotAt(pairTestStart(), 3))
	for _, p := range saved {
		if e.venues.of(p.StrategyID).queryCnt[p.ClientOrderID] == 0 {
			t.Fatalf("the protective reduction for %s was never queried while the elective action stayed unknown", p.StrategyID)
		}
	}
	if n := len(e.actionsByPurpose(pairPurposeProtective)); n != len(saved) {
		t.Fatalf("a recovered protective reduction was replaced instead of queried: %d -> %d", len(saved), n)
	}
	e.assertBooks("protective reductions settled", 0, 0)
	if hedge.Status != basketActionUnresolved {
		t.Fatalf("the unknown elective action must stay unresolved, got %s", hedge.Status)
	}
}

func TestBasketPairDuplicateActionsAreRefusedByTheIndexes(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	elective := e.actionsByPurpose(pairPurposeEntry)[0]
	db := e.db()
	sid, _ := db.toStorageID(pairTestHL)
	insert := func(id, purpose, provider, account, instrument, cloid string) error {
		_, err := db.db.Exec(`INSERT INTO basket_member_actions (action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq, order_side, position_side, status, requested_qty, provider, account_key, client_order_id, created_at_ms, updated_at_ms, purpose, group_mode, instrument, request_json)
			VALUES (?, 'paper', '', ?, 1, ?, 'BTC', 'reduce', ?, 'sell', 'long', 'submitting', 0.01, ?, ?, ?, 1, 1, ?, 'pair', ?, '{}')`,
			id, pairTestID, sid, len(id), provider, account, cloid, purpose, instrument)
		return err
	}
	if err := insert("dup-elective", pairPurposeExit, entryProviderHyperliquid, "synthetic:hl-research", "BTC", "c-1"); err == nil {
		t.Fatalf("a second unresolved elective action for pair %s must be refused beside %s", pairTestID, elective.ActionID)
	}
	if err := insert("prot-hl", pairPurposeProtective, entryProviderHyperliquid, "synthetic:hl-research", "BTC", "c-2"); err != nil {
		t.Fatalf("a protective reduction beside the unresolved elective action must be allowed: %v", err)
	}
	if err := insert("prot-hl-2", pairPurposeKillSwitch, entryProviderHyperliquid, "synthetic:hl-research", "BTC", "c-3"); err == nil {
		t.Fatal("a second unresolved protective reduction on one account and instrument must be refused")
	}
	if err := insert("prot-okx", pairPurposeOperator, entryProviderOKX, "synthetic:okx-research", "BTC-USDT-SWAP", "c-4"); err != nil {
		t.Fatalf("the other member's protective reduction must be allowed: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE status IN ('reserved','submitting','unresolved','settling')`); n != 3 {
		t.Fatalf("want one elective and two protective unresolved rows, got %d", n)
	}
}

func TestBasketPairHoldAgeAndRecoveryAgeBoundaries(t *testing.T) {
	t.Run("hold age", func(t *testing.T) {
		e := newPairTestEnv(t, func(root map[string]interface{}) {
			pairTestPair(root)["limits"].(map[string]interface{})["max_hold_age_ms"] = 600000
		})
		e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
			if r.Lifecycle == pairLifecycleMatched {
				return pairHoldResult(r, "keep"), nil
			}
			return pairEnterResult(r, 0.06), nil
		}
		e.slotStep(0)
		e.slotStep(1)
		first := e.rt().Pair.Progress.FirstFillMs
		e.step(time.UnixMilli(first + 600000 - 1).UTC())
		if e.lifecycle() != pairLifecycleMatched {
			t.Fatalf("one millisecond before the hold age the pair must stay matched, got %s", e.lifecycle())
		}
		e.step(time.UnixMilli(first + 600000).UTC())
		e.assertBooks("hold age exit", 0, 0)
		if len(e.actionsByPurpose(pairPurposeExit)) != 2 {
			t.Fatalf("the hold-age exit must reduce both legs")
		}
	})
	t.Run("recovery age", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		e.externalBookChange(pairTestHL, 0.01)
		e.step(pairSlotAt(pairTestStart(), 1))
		start := e.rt().Pair.Progress.RecoveryStartMs
		if e.lifecycle() != pairLifecycleRecovery || start == 0 {
			t.Fatalf("an unexplained book change must open recovery, got %s", e.lifecycle())
		}
		recoveryAge := e.rt().Pair.Limits.RecoveryAgeMs
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "reduce-only rejected"})
		e.step(time.UnixMilli(start + recoveryAge - 1).UTC())
		if e.lifecycle() != pairLifecycleRecovery {
			t.Fatalf("one millisecond before the recovery age the pair must stay in recovery, got %s", e.lifecycle())
		}
		e.step(time.UnixMilli(start + recoveryAge).UTC())
		if e.lifecycle() != pairLifecycleOperatorHeld {
			t.Fatalf("at the recovery age the pair must be operator held, got %s", e.lifecycle())
		}
		var raw string
		if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		saved, _ := parsePairProgress(raw)
		if saved.Lifecycle != pairLifecycleOperatorHeld || !saved.alerted(fmt.Sprintf("operator-held|%d", start)) {
			t.Fatalf("the operator-held status and its one alert key must be durable: %+v", saved)
		}
	})
}

func TestBasketPairProtectiveRetryBackoff(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	e.externalBookChange(pairTestHL, 0.01)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "venue busy"})
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueFilled})
	at := pairSlotAt(pairTestStart(), 1)
	e.step(at)
	r := e.rt().Pair.Progress.Retries[pairTestOKX]
	if r.Count != 1 || r.RetryAtMs != at.UnixMilli()+5000 {
		t.Fatalf("the first proven failed reduction must back off by retry_initial_ms: %+v", r)
	}
	prot := len(e.actionsByPurpose(pairPurposeProtective))
	e.step(time.UnixMilli(r.RetryAtMs - 1).UTC())
	if got := len(e.actionsByPurpose(pairPurposeProtective)); got != prot {
		t.Fatalf("a retry before its retry time sent a reduction (%d -> %d)", prot, got)
	}
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Terminal: true, Detail: "venue busy"})
	e.step(time.UnixMilli(r.RetryAtMs).UTC())
	acts := e.actionsByPurpose(pairPurposeProtective)
	if len(acts) != prot+1 || acts[len(acts)-1].ClientOrderID == acts[len(acts)-2].ClientOrderID {
		t.Fatalf("the retry at its retry time must take a new action and client identity")
	}
	r2 := e.rt().Pair.Progress.Retries[pairTestOKX]
	if r2.Count != 2 || r2.RetryAtMs != r.RetryAtMs+10000 {
		t.Fatalf("the second failure must double the backoff: %+v", r2)
	}
}

func TestBasketPairProtectiveExceptionRecoversThroughFaults(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	e.externalBookChange(pairTestHL, 0)
	e.venues.okx.mu.Lock()
	e.venues.okx.valErr = errors.New("quote outage")
	e.venues.okx.mu.Unlock()
	for _, id := range []string{pairTestHL, pairTestOKX} {
		sc := e.bc.members[id]
		sc.Paused = true
		e.bc.members[id] = sc
	}
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.step(pairSlotAt(pairTestStart(), 2))
	p := e.rt().Pair.Progress
	if p.Lifecycle != pairLifecycleRecovery || !p.IncreasesFrozen || len(p.Exceptions) == 0 {
		t.Fatalf("a unilateral close must record a durable exception and freeze increases: %+v", p)
	}
	prot := e.actionsByPurpose(pairPurposeProtective)
	if len(prot) != 1 || prot[0].StrategyID != pairTestOKX || prot[0].Direction != basketDirectionReduce {
		t.Fatalf("the survivor must reduce toward zero under pause and a quote outage: %+v", prot)
	}
	e.restart()
	if e.rt().Pair.Progress.Lifecycle != pairLifecycleRecovery {
		t.Fatalf("recovery must stay durable through a restart")
	}
	survivor := e.actionsByPurpose(pairPurposeProtective)[0]
	pairQueryVenue(e, survivor, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: survivor.RequestedQty, CumNotional: survivor.RequestedQty * 60100, Terminal: true})
	e.venues.okx.mu.Lock()
	e.venues.okx.valErr = nil
	e.venues.okx.mu.Unlock()
	e.step(pairSlotAt(pairTestStart(), 3))
	e.assertBooks("survivor flat", 0, 0)
	e.step(pairSlotAt(pairTestStart(), 4))
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a flat, resolved and accounted pair must return to flat, got %s", e.lifecycle())
	}
	if n := e.bookedTrades(pairTestHL); n != 1 {
		t.Fatalf("the closed side was reopened: %d hyperliquid trades", n)
	}
}

func TestBasketPairKillSwitchLatchKeepsFlatteningPriority(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	e.externalBookChange(pairTestHL, 0.01)
	e.mu.Lock()
	prs := e.state.partitionRisk(e.rt().Partition)
	prs.KillSwitchActive = true
	e.mu.Unlock()
	e.step(pairSlotAt(pairTestStart(), 1))
	if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 0 {
		t.Fatalf("a latched kill switch owns flattening; the coordinator sent %d reductions", n)
	}
	if e.lifecycle() != pairLifecycleRecovery {
		t.Fatalf("the exception must still be recorded under the latch, got %s", e.lifecycle())
	}
}

func TestBasketPairReachableFillBoundsAtTheirLimits(t *testing.T) {
	cases := []struct {
		name     string
		cap      float64
		firstQty float64
	}{
		{"unmatched cap equality passes the whole first leg", 0.06 * 60400, 0.06},
		{"one dollar below cuts the first leg to the legal lot", 0.06*60400 - 1, 0.05},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, func(root map[string]interface{}) {
				pairTestPair(root)["limits"].(map[string]interface{})["unmatched_cap_usd"] = tc.cap
			})
			e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
				if r.Lifecycle == pairLifecycleMatched {
					return pairHoldResult(r, "keep"), nil
				}
				return pairEnterResult(r, 0.06), nil
			}
			e.slotStep(0)
			first := e.actionsByPurpose(pairPurposeEntry)[0]
			if first.StrategyID != pairTestOKX || !basketQtyEqual(first.RequestedQty, tc.firstQty) || !basketQtyEqual(first.NativeQty, tc.firstQty/0.01) {
				t.Fatalf("first leg %s %.10g (%.10g contracts), want okx %.10g", first.StrategyID, first.RequestedQty, first.NativeQty, tc.firstQty)
			}
			bound := e.rt().Pair.Progress.LastBound
			if bound == nil || bound.Readings["unmatched_usd"] > tc.cap*(1+1e-9)+1e-9 {
				t.Fatalf("the recorded bound reading must stay within the cap: %+v", bound)
			}
			for i := 1; i < 6; i++ {
				e.slotStep(i)
			}
			e.assertBooks("completed in bounded tranches", 0.06, -0.06)
		})
	}
	t.Run("pending protective fills and collateral count", func(t *testing.T) {
		e := newPairTestEnv(t, func(root map[string]interface{}) {
			pairTestStrategy(root, pairTestOKX)["capital"] = 100
		})
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
			t.Fatalf("a first leg above the member's free collateral must not be sent, got %d actions", n)
		}
	})
	t.Run("no conservative upper valuation holds", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.venues.okx.val.UpperPx = 0
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
			t.Fatalf("a missing upper valuation must hold every elective action, got %d", n)
		}
	})
	t.Run("stale quote at dispatch holds", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		at := pairTestStart()
		e.slot(at, "in-0")
		now := at.Add(time.Second)
		e.venues.okx.val.EvidenceMs = now.UnixMilli() - 5000
		e.step(now)
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 1 {
			t.Fatalf("a quote exactly at its maximum age must pass, got %d actions", n)
		}
		e2 := newPairTestEnv(t, nil)
		pairEnterExitRunner(e2, 0.06)
		e2.slot(at, "in-0")
		e2.venues.okx.val.EvidenceMs = now.UnixMilli() - 5001
		e2.step(now)
		if n := len(e2.actionsByPurpose(pairPurposeEntry)); n != 0 {
			t.Fatalf("a quote one millisecond older than its maximum age must hold, got %d actions", n)
		}
		if rb := e2.rt().Rebalances[at.UnixMilli()]; rb.Status != basketRebalanceHeld {
			t.Fatalf("a frozen decision with stale quotes at its first dispatch must end held, got %s", rb.Status)
		}
	})
	t.Run("settlement conversion and contract multiplier", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.venues.okx.val.Conversions = map[string]float64{"USDT": 0.999}
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		var px float64
		for _, tr := range e.state.Strategies[pairTestOKX].TradeHistory {
			if tr.Symbol == "BTC" {
				px = tr.Price
			}
		}
		if math.Abs(px-59700*0.999) > 1e-6 {
			t.Fatalf("the booked price must convert the settlement currency once, got %v", px)
		}
		a := e.actionsByPurpose(pairPurposeEntry)[0]
		if !basketQtyEqual(a.NativeQty, 6) || a.ContractSize != 0.01 {
			t.Fatalf("the okx request must carry 6 contracts of 0.01 base, got %v x %v", a.NativeQty, a.ContractSize)
		}
	})
}

func TestBasketPairExitAlternatesBoundedTranches(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		pairTestPair(root)["limits"].(map[string]interface{})["unmatched_cap_usd"] = 1300
	})
	pairEnterExitRunner(e, 0.06)
	for i := 0; i < 14 && !(basketQtyEqual(e.signed(pairTestHL), 0.06) && basketQtyEqual(e.signed(pairTestOKX), -0.06)); i++ {
		e.slotStep(i)
	}
	e.assertBooks("entered in bounded tranches", 0.06, -0.06)
	entries := len(e.actionsByPurpose(pairPurposeEntry))
	if entries < 4 {
		t.Fatalf("a 1300 dollar unmatched cap must split the entry into alternating tranches, got %d", entries)
	}
	for i := 14; i < 30 && (e.signed(pairTestHL) != 0 || e.signed(pairTestOKX) != 0); i++ {
		e.slotStep(i)
	}
	e.assertBooks("exited", 0, 0)
	exits := e.actionsByPurpose(pairPurposeExit)
	if len(exits) < 4 {
		t.Fatalf("the exit must alternate bounded tranches, got %d", len(exits))
	}
	for i := 1; i < len(exits); i++ {
		if exits[i].StrategyID == exits[i-1].StrategyID && exits[i].RequestedQty > 0.03 {
			t.Fatalf("exit tranche %d repeats %s with %v", i, exits[i].StrategyID, exits[i].RequestedQty)
		}
	}
}

func TestBasketPairNoLegalLotEndsTheEntry(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		pairTestPair(root)["limits"].(map[string]interface{})["unmatched_cap_usd"] = 500
	})
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
		t.Fatalf("no legal okx lot fits a 500 dollar unmatched cap, yet %d actions were reserved", n)
	}
	rb := e.rt().Rebalances[pairTestStart().UnixMilli()]
	found := false
	for _, d := range rb.Outcome.Deviations {
		if d.Kind == "no_legal_tranche" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the slot must record the missing legal tranche: %+v", rb.Outcome)
	}
}

func TestBasketPairConcurrentSavesKeepRecoveryDurable(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.mu.Lock()
			defer e.mu.Unlock()
			_ = e.store.SavePartition(e.state, e.rt().Partition)
		}()
	}
	pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true})
	e.slotStep(1)
	wg.Wait()
	e.restart()
	e.slotStep(2)
	e.assertBooks("one booking", 0.06, -0.06)
	if n := e.bookedTrades(pairTestOKX); n != 1 {
		t.Fatalf("concurrent saves must not duplicate the fill: %d trades", n)
	}
	var progress string
	if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&progress); err != nil {
		t.Fatal(err)
	}
	var p pairProgress
	if err := json.Unmarshal([]byte(progress), &p); err != nil || p.Lifecycle != pairLifecycleMatched {
		t.Fatalf("the saved pair progress after concurrent saves is %q (%v)", progress, err)
	}
}

func TestBasketPairCollateralAndPartitionGatesAtTheirLimits(t *testing.T) {
	qty, upper, imr, feeRate := 0.06, 60400.0, 0.5, 0.0005
	required := qty*upper*imr + qty*upper*1*feeRate
	for _, tc := range []struct {
		name    string
		capital float64
		qty     float64
	}{
		{"collateral equal to the requirement passes the whole first leg", required, 0.06},
		{"one cent below the requirement cuts the first leg to the next legal lot", required - 0.01, 0.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, func(root map[string]interface{}) {
				pairTestStrategy(root, pairTestOKX)["capital"] = tc.capital
			})
			pairEnterHoldRunner(e, 0.06)
			e.slotStep(0)
			acts := e.actionsByPurpose(pairPurposeEntry)
			if len(acts) != 1 || !basketQtyEqual(acts[0].RequestedQty, tc.qty) {
				t.Fatalf("capital %.6f against a %.6f requirement: first legs %+v, want one of %.10g", tc.capital, required, acts, tc.qty)
			}
		})
	}
	t.Run("partition notional cap holds increases", func(t *testing.T) {
		e := newPairTestEnv(t, func(root map[string]interface{}) {
			root["portfolio_risk"] = map[string]interface{}{"max_drawdown_pct": 25, "max_notional_usd": 1000}
		})
		pairEnterHoldRunner(e, 0.06)
		e.slotStep(0)
		if n := len(e.actionsByPurpose(pairPurposeEntry)); n != 0 {
			t.Fatalf("the partition notional cap must hold pair increases, got %d actions", n)
		}
	})
}

func TestBasketPairStaleQueryAnswerIsDiscarded(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	pairAnswerVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true})
	e.venues.okx.onQuery = func(*basketAction) {
		e.venues.okx.onQuery = nil
		a.FeeEvidenceVersion = 7
	}
	e.step(pairSlotAt(pairTestStart(), 1))
	if a.AppliedQty != 0 || a.Status != basketActionUnresolved {
		t.Fatalf("an answer for an action that changed during the unlocked query must be discarded: %+v", a)
	}
	e.assertBooks("stale answer discarded", 0, 0)
	e.step(pairSlotAt(pairTestStart(), 2))
	e.assertBooks("the next query applies", 0, -0.06)
}
