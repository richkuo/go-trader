package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (e *pairTestEnv) savedPositionMatch() *pairPositionMatch {
	e.t.Helper()
	var raw string
	if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
		e.t.Fatalf("read saved pair progress: %v", err)
	}
	p, err := parsePairProgress(raw)
	if err != nil {
		e.t.Fatalf("saved pair progress: %v", err)
	}
	return p.PositionMatch
}

func (e *pairTestEnv) alertsWithPrefix(prefix string) int {
	n := 0
	for _, k := range e.rt().Pair.Progress.Alerts {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func (e *pairTestEnv) entryActions() int {
	return e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair' AND purpose = 'pair_entry'`)
}

func TestBasketPairPositionMismatchOpensOneRecoveryEpisodeAndAMatchClearsOnlyItsHold(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	e.venues.hl.setVenueBase(0.05)
	at := e.slotStep(0)
	if n := e.entryActions(); n != 0 {
		t.Fatalf("a venue position outside the reachable range authorized %d increases", n)
	}
	pm := e.savedPositionMatch()
	if pm == nil || pm.Status != pairPositionMismatch || pm.EpisodeMs == 0 {
		t.Fatalf("the saved barrier is %+v, want a mismatch with an episode", pm)
	}
	if e.lifecycle() != pairLifecycleRecovery || !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("lifecycle %s, want recovery with increases frozen", e.lifecycle())
	}
	if n := e.alertsWithPrefix("position_match|"); n != 1 {
		t.Fatalf("one mismatch episode sent %d barrier alerts, want 1", n)
	}
	e.step(at.Add(2 * time.Minute))
	if n := e.alertsWithPrefix("position_match|"); n != 1 {
		t.Fatalf("a repeated mismatch in the same episode sent %d barrier alerts, want 1", n)
	}
	e.venues.hl.setVenueBase(0)
	e.step(at.Add(4 * time.Minute))
	pm = e.savedPositionMatch()
	if pm == nil || pm.Status != pairPositionMatched || pm.EpisodeMs != 0 {
		t.Fatalf("a fresh matching read must clear the barrier, got %+v", pm)
	}
	if e.lifecycle() == pairLifecycleRecovery && !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatal("a matching read must never clear the recovery freeze")
	}
	if !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatal("the recovery freeze from the mismatch was cleared by the barrier")
	}
	e.venues.hl.setVenueBase(0.05)
	e.step(at.Add(6 * time.Minute))
	if n := e.alertsWithPrefix("position_match|"); n != 2 {
		t.Fatalf("a new mismatch episode after a match must alert again, got %d barrier alerts", n)
	}
	e.restart()
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMismatch {
		t.Fatalf("the barrier must survive a restart, got %+v", pm)
	}
	if line := pairStartupLine(e.rt()); !strings.Contains(line, "POSITION MATCH mismatch") {
		t.Fatalf("the startup line must show the saved barrier, got %q", line)
	}
}

func TestBasketPairPositionBarrierSurvivesFlatResetAndNeedsAReadAfterRestart(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	e.runner.respond = func(req pairPolicyRequest) (pairPolicyResult, error) { return pairHoldResult(req, "no edge"), nil }
	e.venues.hl.setVenueBase(0.02)
	at := e.slotStep(0)
	if e.lifecycle() != pairLifecycleRecovery {
		t.Fatalf("lifecycle %s, want recovery after the mismatch", e.lifecycle())
	}
	e.venues.hl.setVenueBase(0)
	e.step(at.Add(2 * time.Minute))
	if e.lifecycle() != pairLifecycleRecovery {
		t.Fatalf("the recovery must stay open while the saved barrier is a mismatch, got %s", e.lifecycle())
	}
	e.step(at.Add(150 * time.Second))
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a flat pair with terminal actions must reset to flat, got %s", e.lifecycle())
	}
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMatched {
		t.Fatalf("the flat reset must keep the barrier record, got %+v", pm)
	}
	e.restart()
	if st := e.rt().pairPositions; st != nil && st.auth != nil {
		t.Fatal("a restart must not carry a saved match forward as authority")
	}
	if why := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now)); why == "" {
		t.Fatal("increases must hold after a restart until a new venue read matches")
	}
	before := e.venues.hl.posReads
	e.step(at.Add(3 * time.Minute))
	if e.venues.hl.posReads == before {
		t.Fatal("the first step after a restart must read the venue positions")
	}
	if st := e.rt().pairPositions; st == nil || st.auth == nil {
		t.Fatal("a fresh matching read after the restart must authorize increases again")
	}
}

func TestBasketPairPositionBadReadsHoldIncreasesWhileQueriesContinue(t *testing.T) {
	stale := func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
		return pairTestVenuePosition(m, 0, nowMs-5001, nowMs-5001)
	}
	cases := map[string]func(m pairContractMember, nowMs int64) (pairVenuePosition, error){
		"read error": func(pairContractMember, int64) (pairVenuePosition, error) {
			return pairVenuePosition{}, fmt.Errorf("venue unreachable")
		},
		"older than quote_age_ms": stale,
		"observed in the future": func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			return pairTestVenuePosition(m, 0, nowMs+1, nowMs+1)
		},
		"hash does not match": func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			p, err := pairTestVenuePosition(m, 0, nowMs, nowMs)
			p.SignedNativeQty = "1"
			return p, err
		},
		"contract size differs": func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			p, _ := pairTestVenuePosition(m, 0, nowMs, nowMs)
			p.ContractSize = "1"
			p.EvidenceHash = ""
			return p.sealed()
		},
	}
	for name, hook := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPairLiveShapedEnv(t, &pairNativeClosers{})
			e.venues.okx.position = hook
			at := e.slotStep(0)
			if n := e.entryActions(); n != 0 {
				t.Fatalf("a refused position read authorized %d increases", n)
			}
			if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionFailed {
				t.Fatalf("the saved barrier is %+v, want failed", pm)
			}
			if e.lifecycle() == pairLifecycleRecovery {
				t.Fatal("a failed read holds increases and opens no recovery")
			}
			_ = at
		})
	}
	t.Run("exactly quote_age_ms old passes", func(t *testing.T) {
		e := newPairLiveShapedEnv(t, &pairNativeClosers{})
		e.venues.okx.position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			return pairTestVenuePosition(m, 0, nowMs-5000, nowMs-5000)
		}
		e.venues.hl.position = e.venues.okx.position
		e.slotStep(0)
		if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMatched {
			t.Fatalf("a read exactly quote_age_ms old must match at the step, got %+v", pm)
		}
	})
	t.Run("a timed-out read holds and the saved query still runs", func(t *testing.T) {
		e := newPairLiveShapedEnv(t, &pairNativeClosers{})
		pairEnterExitRunner(e, 0.06)
		pairOpenVenue(e, pairTestOKX, pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59700, Terminal: true}, 1.5, "USDT", true))
		e.slotStep(0)
		e.assertBooks("first leg", 0, -0.06)
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "timeout"})
		e.slotStep(1)
		var pending *basketAction
		for _, a := range e.actionsByPurpose(pairPurposeEntry) {
			if a.StrategyID == pairTestHL && basketActionPending(a.Status) {
				pending = a
			}
		}
		if pending == nil {
			t.Fatal("the hedge must stay unresolved after an unknown outcome")
		}
		e.venues.hl.position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			<-ctx.Done()
			return pairVenuePosition{}, ctx.Err()
		}
		pairQueryVenue(e, pending, pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 60100, Terminal: true}, 1.5, "USDC", true))
		before := e.venues.hl.queryCnt[pending.ClientOrderID]
		e.step(pairSlotAt(pairTestStart(), 2).Add(time.Second))
		if e.venues.hl.queryCnt[pending.ClientOrderID] == before {
			t.Fatal("a failed position read suppressed the saved-action query")
		}
		e.assertBooks("settled through the query", 0.06, -0.06)
	})
}

func TestBasketPairPositionGenerationChangeDiscardsTheRead(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	calls := 0
	e.venues.hl.position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
		calls++
		if calls == 1 {
			e.mu.Lock()
			e.rt().Pair.Progress.Lifecycle = pairLifecycleOpening
			e.mu.Unlock()
		}
		return pairTestVenuePosition(m, 0, nowMs, nowMs)
	}
	at := pairSlotAt(pairTestStart(), 0)
	e.slot(at, "in-0")
	e.now = at.Add(time.Second)
	e.bc.beginCycle()
	e.observe("")
	e.mu.Lock()
	in, why := e.bc.pairReadPositions(e.rt(), e.inputs(e.now), "")
	e.mu.Unlock()
	_ = in
	if why == "" || e.rt().pairPositions.auth != nil {
		t.Fatalf("a read that saw the pair records change must be discarded, got reason %q", why)
	}
	if pm := e.rt().Pair.Progress.PositionMatch; pm != nil && pm.Status == pairPositionMatched {
		t.Fatalf("a discarded read must not record a match: %+v", pm)
	}
}

func TestBasketPairPositionReachableUnresolvedFillsMatchWithoutAdoption(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterExitRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "timeout"})
	at := e.slotStep(0)
	e.assertBooks("unknown first leg", 0, 0)
	e.venues.okx.setVenueBase(-0.03)
	e.step(at.Add(2 * time.Minute))
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMatched {
		t.Fatalf("a venue position inside the unresolved first leg's reachable range must match, got %+v", pm)
	}
	e.assertBooks("never adopted into the book", 0, 0)
	e.venues.okx.setVenueBase(-0.07)
	e.step(at.Add(4 * time.Minute))
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMismatch {
		t.Fatalf("a venue position beyond the unresolved remaining quantity must mismatch, got %+v", pm)
	}
	e.venues.okx.setVenueBase(0.01)
	e.step(at.Add(6 * time.Minute))
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMismatch {
		t.Fatalf("a venue position on the wrong side of the unresolved sell must mismatch, got %+v", pm)
	}
	e.assertBooks("still never adopted", 0, 0)
}
