//go:build pyintegration

package main

import (
	"testing"
	"time"
)

func TestBasketPairValuationRealHostRiskUsesTheMemberViews(t *testing.T) {
	p := newOKXPairStepEnv(t, pairSetLimit("decision_cadence_ms", 500))
	e := p.e
	e.collect = true
	e.quotes.enable()
	feed := pairPyFeed(t, e.cfg)
	hl, okx := pairPySC(e, pairTestHL), p.sc()
	pairHostRunner(e, 2)
	h := newPairTestHost(e, time.Minute)
	var captured map[RiskPartition]*scopeCycleRisk
	var memberStatus []pairValueStatus
	var memberReasons []string
	h.manage = func(_ time.Time, risk map[RiskPartition]*scopeCycleRisk) {
		pairPyStep(t, e, hl, feed, nil)
		runOKXStep(p.context(okx), okx, okxPairManagementCheck(pairPyManagementInput(60000), p.notifier))
		captured = risk
		memberStatus, memberReasons = nil, nil
		e.mu.Lock()
		pm := e.bc.pairMarksLocked()
		for _, id := range []string{pairTestHL, pairTestOKX} {
			sc := e.bc.members[id]
			_, reason, st := checkPairMemberRisk(&sc, e.state.Strategies[id], pm, nil, nil)
			memberStatus, memberReasons = append(memberStatus, st), append(memberReasons, reason)
		}
		e.mu.Unlock()
	}
	t0 := pairTestStart()
	h.runUntil(t0.Add(3 * time.Second))
	e.assertBooks("matched on real management output", 0.06, -0.06)
	at := t0.Add(time.Minute)
	h.run(at)
	if !h.runs[len(h.runs)-1].Managed || captured == nil {
		t.Fatal("the final host run must manage the members after preparing the view")
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		if obs, ok := e.rt().observations[id]; !ok || obs.AtMs != at.UnixMilli() || indicatorsATRValue(obs.Indicators) <= 0 {
			t.Fatalf("member %s must carry the real management echo of the final run: %+v", id, obs)
		}
	}
	part := e.rt().Partition
	sr := captured[part]
	e.mu.RLock()
	pm := e.bc.pairMarksLocked()
	views := e.bc.pairViewsLocked(at)
	var want float64
	for _, id := range []string{pairTestHL, pairTestOKX} {
		v, st := portfolioValueMarked(e.state.Strategies[id], nil, pm)
		if !st.complete() {
			t.Fatalf("member %s view is incomplete after a real host run: %s", id, st.label())
		}
		want += v
	}
	legacy := measureScopeCycleRisk(part, partitionRiskConfig(e.cfg, part), e.cfg.Strategies, e.state, h.marks, nil, nil, true, false, at)
	e.mu.RUnlock()
	if sr == nil || !sr.PairValuation.complete() || !pairViewNear(sr.TotalPV, want) {
		t.Fatalf("host risk must value the partition from the prepared views: %+v want %v", sr, want)
	}
	if legacy.PairValuation.complete() || !views[pairTestHL].MarkValid || !views[pairTestOKX].MarkValid || views[pairTestHL].MarkUSD == views[pairTestOKX].MarkUSD {
		t.Fatalf("a nil-view risk read must report the pair incomplete and the two members must carry distinct marks: %s, %v/%v", legacy.PairValuation.label(), views[pairTestHL].MarkUSD, views[pairTestOKX].MarkUSD)
	}
	for i, st := range memberStatus {
		if !st.complete() || memberReasons[i] != "" {
			t.Fatalf("the member circuit-breaker reading must be complete on the prepared view, got %s %q", st.label(), memberReasons[i])
		}
	}
}
