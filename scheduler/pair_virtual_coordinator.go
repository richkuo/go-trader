package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

func (bc *basketCoordinator) attachPairVirtualVenues() error {
	if !bc.active() {
		return nil
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	clock := func() time.Time {
		c := bc.pairClock
		if c == nil {
			c = tradingNow
		}
		return c()
	}
	var errs []string
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		for _, m := range rt.Contract.pairMembersSorted() {
			sc, ok := bc.members[m.MemberID]
			if !ok {
				continue
			}
			v, err := bc.venueFor(sc)
			if err != nil {
				errs = append(errs, fmt.Sprintf("pair %s member %s: %v", id, m.MemberID, err))
				continue
			}
			vv, ok := v.(*pairVirtualVenue)
			if !ok {
				continue
			}
			if err := vv.attach(pairVirtualAttachment{Store: bc.store, Runtime: rt, Config: sc, Clock: clock}); err != nil {
				errs = append(errs, fmt.Sprintf("pair %s member %s: %v", id, m.MemberID, err))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("pair virtual venue attachment refused:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func (bc *basketCoordinator) pairVirtualVenueFor(id string) *pairVirtualVenue {
	sc, ok := bc.members[id]
	if !ok {
		return nil
	}
	v, err := bc.venueFor(sc)
	if err != nil {
		return nil
	}
	vv, _ := v.(*pairVirtualVenue)
	return vv
}

func (bc *basketCoordinator) pairNativeRows(rt *basketRuntime, id string) []pairInventoryRow {
	if rt.Pair == nil || rt.Pair.funding == nil {
		return nil
	}
	m := rt.Pair.funding.members[id]
	if m == nil {
		return nil
	}
	return append([]pairInventoryRow(nil), m.rows...)
}

func (bc *basketCoordinator) notePairVirtualInventory(rt *basketRuntime, v pairVenue, id string) {
	if vv, ok := v.(*pairVirtualVenue); ok {
		vv.noteNativeInventory(bc.pairNativeRows(rt, id))
	}
}

func pairVirtualOutcomeFuture(v pairVenue, out basketVenueOutcome, nowMs int64) string {
	if _, ok := v.(*pairVirtualVenue); !ok {
		return ""
	}
	if out.ObservedAtMs > nowMs {
		return fmt.Sprintf("the virtual venue observed its answer at %d, after the coordinator time %d", out.ObservedAtMs, nowMs)
	}
	for _, f := range out.Fills {
		if f.VisibleMs > nowMs {
			return fmt.Sprintf("virtual fill %s is visible at %d, after the coordinator time %d", f.FillID, f.VisibleMs, nowMs)
		}
	}
	return ""
}

func (bc *basketCoordinator) pairVirtualConservation(rt *basketRuntime) string {
	var bad []string
	for _, m := range rt.Contract.pairMembersSorted() {
		vv := bc.pairVirtualVenueFor(m.MemberID)
		if vv == nil {
			continue
		}
		per, total, ok := vv.executedBase()
		if !ok {
			bad = append(bad, m.MemberID+" virtual venue is not attached")
			continue
		}
		if rt.Pair.funding == nil || rt.Pair.funding.members[m.MemberID] == nil {
			bad = append(bad, m.MemberID+" has no inventory journal for the conservation check")
			continue
		}
		native := 0.0
		for _, r := range rt.Pair.funding.members[m.MemberID].rows {
			if r.Origin == pairInventoryOriginNative {
				native += pairRatFloat(pairRatOf(r.Delta))
			}
		}
		book := basketPositionSigned(bc.state.Strategies[m.MemberID], m.Symbol)
		external := book
		for _, id := range rt.sortedActionIDs() {
			a := rt.Actions[id]
			if a.StrategyID != m.MemberID || a.recoveryNote().NativeBooked {
				continue
			}
			executed := 0.0
			if q := per[a.ClientOrderID]; q != nil {
				executed = pairRatFloat(q)
			}
			if math.Abs(executed)+1e-12 < a.ConfirmedQty*(1-1e-9) {
				bad = append(bad, fmt.Sprintf("%s action %s confirms %.10g, the virtual venue executed %.10g", m.MemberID, a.ActionID, a.ConfirmedQty, math.Abs(executed)))
			}
			external += executed - a.signedDelta()*a.AppliedQty
		}
		internal := pairRatFloat(total) + native
		if math.Abs(internal-external) > 1e-9*math.Max(1, math.Abs(internal)) {
			bad = append(bad, fmt.Sprintf("%s simulated inventory %.10g (virtual executions plus native book changes) differs from the book with confirmed unapplied and hidden executions %.10g", m.MemberID, internal, external))
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, "; ")
}

func (bc *basketCoordinator) checkPairVirtualConservation(rt *basketRuntime) {
	why := bc.pairVirtualConservation(rt)
	if why == "" {
		return
	}
	bc.pairLogf(rt, "virtual inventory conservation holds increases: %s; queries, settlement and native protection continue", why)
	bc.pairAlert(rt, "virtual_conservation|"+why, fmt.Sprintf("%s: virtual inventory conservation failed (%s); increases hold until it agrees, and queries, settlement and native protection continue", rt.Contract.BasketID, why), true)
}
