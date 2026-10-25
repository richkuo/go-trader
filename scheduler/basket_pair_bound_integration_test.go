package main

import (
	"encoding/json"
	"testing"
	"time"
)

func pairBoundResult(req pairPolicyRequest, kind string, hlQty, hlBound, okxBound float64) pairPolicyResult {
	orientation := pairOrientationLongHLShortOKX
	if hlQty < 0 {
		orientation = pairOrientationLongOKXShortHL
	}
	res := pairPolicyResult{V: pairPolicyRequestVersion, PairID: req.PairID, SlotMs: req.SlotMs, ContractFingerprint: req.ContractFingerprint, InputFingerprint: req.InputFingerprint,
		Status: kind, Decision: json.RawMessage(`{"reason":"bound fixture"}`),
		Targets: map[string]pairPolicyMemberTarget{pairTestHL: {TargetBaseQty: hlQty, PriceBound: hlBound}, pairTestOKX: {TargetBaseQty: -hlQty, PriceBound: okxBound}}}
	if kind == pairPolicyStatusEnter {
		res.Orientation = orientation
	} else {
		res.Targets = map[string]pairPolicyMemberTarget{pairTestHL: {TargetBaseQty: 0, PriceBound: hlBound}, pairTestOKX: {TargetBaseQty: 0, PriceBound: okxBound}}
	}
	return res
}

func (e *pairTestEnv) savedTargets(slot time.Time) *pairTargets {
	e.t.Helper()
	var stored string
	if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, slot.UnixMilli()).Scan(&stored); err != nil {
		e.t.Fatalf("read the saved decision: %v", err)
	}
	got, err := parsePairTargets(stored)
	if err != nil || got == nil {
		e.t.Fatalf("the saved decision %q is unreadable: %v", stored, err)
	}
	return got
}

func (e *pairTestEnv) requestBound(a *basketAction) float64 {
	e.t.Helper()
	var req pairActionRequest
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err != nil {
		e.t.Fatalf("action %s request: %v", a.ActionID, err)
	}
	return req.PriceBound
}

func (e *pairTestEnv) electiveActions() int {
	return e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair' AND purpose IN ('pair_entry','pair_exit')`)
}

func TestBasketPairBoundRoundingFreezesAndDispatchesTheSavedBound(t *testing.T) {
	e := newPairTestEnv(t, nil)
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
		return pairBoundResult(req, pairPolicyStatusEnter, 0.06, 60400.7, 59700.05), nil
	}
	slot0 := e.slotStep(0)
	got := e.savedTargets(slot0)
	if got.Version != 2 || got.Kind != pairPolicyStatusEnter {
		t.Fatalf("the saved decision is version %d kind %s, want version 2 enter", got.Version, got.Kind)
	}
	hl, okx := got.BoundEvidence[pairTestHL], got.BoundEvidence[pairTestOKX]
	if hl.ProposedBound != 60400.7 || hl.RoundedBound != 60400 || hl.Side != "buy" || okx.ProposedBound != 59700.05 || okx.RoundedBound != 59700.1 || okx.Side != "sell" {
		t.Fatalf("bound evidence %+v %+v, want the buy cap rounded down to 60400 and the sell floor rounded up to 59700.1", hl, okx)
	}
	if !pairHexDigestPattern.MatchString(hl.CapabilityHash) || !pairHexDigestPattern.MatchString(okx.CapabilityHash) {
		t.Fatalf("bound evidence names no capability hash: %+v %+v", hl, okx)
	}
	if got.Members[pairTestHL].PriceBound != 60400 || got.Members[pairTestOKX].PriceBound != 59700.1 {
		t.Fatalf("saved targets hold %+v, want the rounded bounds", got.Members)
	}
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 || first[0].StrategyID != pairTestOKX || first[0].PriceBound != 59700.1 || e.requestBound(first[0]) != 59700.1 {
		t.Fatalf("the first leg must send the saved rounded sell floor, got %+v", first)
	}
	e.restart()
	e.slotStep(1)
	all := e.actionsByPurpose(pairPurposeEntry)
	if len(all) != 2 || all[1].StrategyID != pairTestHL || all[1].PriceBound != 60400 || e.requestBound(all[1]) != 60400 {
		t.Fatalf("after a restart the hedge must send the same saved rounded buy cap, got %+v", all)
	}
	e.assertBooks("hedged at the saved bounds", 0.06, -0.06)
}

func TestBasketPairBoundPostRoundingEconomicsHold(t *testing.T) {
	cases := map[string]struct {
		hlQty, hlBound, okxBound float64
		want                     string
	}{
		"minimum notional after rounding": {hlQty: 0.03, hlBound: 333.335, okxBound: 333, want: basketRebalanceHeld},
		"cap at the proposed buy bound":   {hlQty: 0.1, hlBound: 70000.9, okxBound: 40000, want: basketRebalanceHeld},
		"cap equality at the bound":       {hlQty: 0.1, hlBound: 70000, okxBound: 40000, want: basketRebalanceExecuting},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
				return pairBoundResult(req, pairPolicyStatusEnter, tc.hlQty, tc.hlBound, tc.okxBound), nil
			}
			at := pairSlotAt(pairTestStart(), 0)
			e.slot(at, "in-0")
			e.step(at.Add(time.Second))
			rb := e.rt().Rebalances[at.UnixMilli()]
			if rb.Status != tc.want {
				t.Fatalf("slot status %s, want %s (hold %q)", rb.Status, tc.want, rb.HoldReason)
			}
			if tc.want == basketRebalanceHeld {
				if n := e.electiveActions(); n != 0 {
					t.Fatalf("a decision that fails its post-rounding checks reserved %d actions", n)
				}
				if got := e.savedTargets(at); got.Kind != pairPolicyStatusHold || len(got.BoundEvidence) != 0 {
					t.Fatalf("the held decision must be saved as a version 2 hold with no bound evidence, got %+v", got)
				}
			}
		})
	}
}

func TestBasketPairBoundExitCloseTrancheMinimumAfterRounding(t *testing.T) {
	e := newPairTestEnv(t, nil)
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
		if req.Lifecycle == pairLifecycleMatched {
			return pairBoundResult(req, pairPolicyStatusExit, 0, 333.335, 333), nil
		}
		return pairEnterResult(req, -0.03), nil
	}
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", -0.03, 0.03)
	slot2 := e.slotStep(2)
	if rb := e.rt().Rebalances[slot2.UnixMilli()]; rb.Status != basketRebalanceHeld {
		t.Fatalf("an exit whose nonzero close tranche falls below the venue minimum after rounding must hold, got %s", rb.Status)
	}
	if n := len(e.actionsByPurpose(pairPurposeExit)); n != 0 {
		t.Fatalf("the held exit reserved %d exit tranches", n)
	}
	e.assertBooks("unchanged", -0.03, 0.03)
}

func TestBasketPairBoundDispatchNeedsCrossingAndFullDepth(t *testing.T) {
	cases := map[string]struct {
		hlQty, okxBound float64
		dispatched      bool
	}{
		"bound equal to the best bid crosses and depth equality fills": {hlQty: 0.03, okxBound: 60099.9, dispatched: true},
		"bound above the best bid holds the first leg":                 {hlQty: 0.03, okxBound: 60100, dispatched: false},
		"depth inside the bound below the tranche holds":               {hlQty: 0.04, okxBound: 60099.9, dispatched: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPairTestEnv(t, nil)
			e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) {
				return pairBoundResult(req, pairPolicyStatusEnter, tc.hlQty, 60400, tc.okxBound), nil
			}
			at := e.slotStep(0)
			rb := e.rt().Rebalances[at.UnixMilli()]
			n := e.electiveActions()
			if tc.dispatched {
				if n != 1 || rb.Status != basketRebalanceExecuting {
					t.Fatalf("the first leg must dispatch (%d actions, slot %s %q)", n, rb.Status, rb.HoldReason)
				}
				e.assertBooks("first leg", 0, -tc.hlQty)
				return
			}
			if n != 0 || rb.Status != basketRebalanceHeld {
				t.Fatalf("the first leg must hold without a reservation (%d actions, slot %s)", n, rb.Status)
			}
			if e.lifecycle() == pairLifecycleRecovery {
				t.Fatal("a first leg that cannot dispatch must not open recovery")
			}
		})
	}
	t.Run("a hedge that no longer crosses opens recovery at once", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.assertBooks("first leg", 0, -0.06)
		e.quotes.mu.Lock()
		e.quotes.hlAsks = [][2]string{{"61000", "0.5"}, {"61001", "0.6"}, {"61002", "0.7"}, {"61003", "0.8"}, {"61004", "0.9"}, {"61005", "1"}}
		e.quotes.mu.Unlock()
		e.slotStep(1)
		if e.lifecycle() != pairLifecycleRecovery || !e.rt().Pair.Progress.IncreasesFrozen {
			t.Fatalf("lifecycle %s frozen %v, want recovery with increases frozen", e.lifecycle(), e.rt().Pair.Progress.IncreasesFrozen)
		}
		for _, a := range e.actionsByPurpose(pairPurposeEntry) {
			if a.StrategyID == pairTestHL {
				t.Fatalf("a non-crossing hedge was reserved: %+v", a)
			}
		}
		e.assertBooks("unhedged", 0, -0.06)
	})
}

func (e *pairTestEnv) rewriteSavedTargetsV1(slot time.Time) {
	e.t.Helper()
	var stored string
	if err := e.db().db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE deadline_ms = ?`, slot.UnixMilli()).Scan(&stored); err != nil {
		e.t.Fatal(err)
	}
	var legacy map[string]interface{}
	if stored == "" {
		legacy = map[string]interface{}{"kind": pairPolicyStatusEnter, "orientation": pairOrientationLongHLShortOKX, "slot_ms": slot.UnixMilli(),
			"input_fingerprint": e.rt().Rebalances[slot.UnixMilli()].InputFP, "decision": map[string]interface{}{},
			"members": map[string]interface{}{pairTestHL: map[string]interface{}{"target_base_qty": 0.06, "price_bound": 60400}, pairTestOKX: map[string]interface{}{"target_base_qty": -0.06, "price_bound": 59700}}}
	} else if err := json.Unmarshal([]byte(stored), &legacy); err != nil {
		e.t.Fatal(err)
	}
	legacy["version"] = 1
	delete(legacy, "bound_evidence")
	raw, err := json.Marshal(legacy)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db().db.Exec(`UPDATE basket_rebalances SET targets_json = ? WHERE deadline_ms = ?`, string(raw), slot.UnixMilli()); err != nil {
		e.t.Fatal(err)
	}
}

func TestBasketPairBoundLegacyDecisionAuthorizesNoOrder(t *testing.T) {
	t.Run("a frozen version-1 entry holds without an order", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		at := pairSlotAt(pairTestStart(), 0)
		e.slot(at, "in-0")
		e.rewriteSavedTargetsV1(at)
		e.restart()
		e.step(at.Add(time.Second))
		if rb := e.rt().Rebalances[at.UnixMilli()]; rb.Status != basketRebalanceHeld {
			t.Fatalf("the legacy decision slot is %s, want held", rb.Status)
		}
		if n := e.electiveActions(); n != 0 {
			t.Fatalf("a legacy decision authorized %d orders", n)
		}
		if got := e.savedTargets(at); got.Version != 1 {
			t.Fatalf("the legacy record bytes changed to version %d", got.Version)
		}
	})
	t.Run("an active version-1 entry with exposure opens recovery and keeps saved queries", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		slot0 := pairSlotAt(pairTestStart(), 0)
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: pairVenueOpen, CumQty: 0.03, CumNotional: 0.03 * 60000})
		e.slotStep(0)
		pending := e.actionsByPurpose(pairPurposeEntry)
		if len(pending) != 1 || pending[0].Status != basketActionUnresolved {
			t.Fatalf("the first leg must stay unresolved after an open partial fill: %+v", pending)
		}
		e.rewriteSavedTargetsV1(slot0)
		e.restart()
		pending = e.actionsByPurpose(pairPurposeEntry)
		pairQueryVenue(e, pending[0], basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 60000, Terminal: true})
		e.slotStep(1)
		if e.venues.okx.queryCnt[pending[0].ClientOrderID] == 0 {
			t.Fatal("the saved first-leg action must still be queried under a legacy decision")
		}
		e.assertBooks("settled from the saved query", 0, -0.06)
		if e.lifecycle() != pairLifecycleRecovery {
			t.Fatalf("lifecycle %s, want recovery", e.lifecycle())
		}
		for _, a := range e.actionsByPurpose(pairPurposeEntry) {
			if a.StrategyID == pairTestHL {
				t.Fatalf("a legacy decision authorized a hedge: %+v", a)
			}
		}
	})
}
