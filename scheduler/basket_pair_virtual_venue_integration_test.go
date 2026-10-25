package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type pairTestMarket struct {
	mu       sync.Mutex
	events   map[string][]pairObservation
	coverage func(key pairMemberKey, afterMs, throughMs int64) pairCoverage
	err      error
	reads    int
}

func newPairTestMarket() *pairTestMarket {
	return &pairTestMarket{events: map[string][]pairObservation{}}
}

func (m *pairTestMarket) add(id string, obs ...pairObservation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[id] = append(m.events[id], obs...)
}

func (m *pairTestMarket) replace(id string, obs ...pairObservation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[id] = append([]pairObservation(nil), obs...)
}

func (m *pairTestMarket) Events(ctx context.Context, key pairMemberKey, afterMs, throughMs int64) (pairVirtualMarketWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if m.err != nil {
		return pairVirtualMarketWindow{}, m.err
	}
	type ranked struct {
		o   pairObservation
		key int64
		i   int
	}
	var picked []ranked
	var spec pairSourceSpec
	for i, o := range m.events[key.MemberID] {
		if s, ok := pairSourceSpecFor(o.Key.Provider, o.Source); ok {
			spec = s
		}
		venue := *o.VenueTimeMs
		if venue <= afterMs || venue > throughMs || o.FirstReceiptMs > throughMs {
			continue
		}
		k := o.FirstReceiptMs
		if !spec.Point {
			k = o.SourceCutoffMs
		}
		picked = append(picked, ranked{o: pairCloneObservation(o), key: k, i: i})
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].key < picked[j].key })
	w := pairVirtualMarketWindow{Events: []pairObservation{}}
	for _, p := range picked {
		w.Events = append(w.Events, p.o)
	}
	if m.coverage != nil {
		w.Coverage = m.coverage(key, afterMs, throughMs)
	} else {
		if spec.Contract == "" {
			spec, _ = pairSourceSpecFor(key.Provider, map[string]string{entryProviderOKX: pairSourcePublicSnapshot, entryProviderHyperliquid: pairSourcePublicSnapshot}[key.Provider])
		}
		w.Coverage = pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
			FromMs: afterMs, ThroughMs: throughMs, Status: pairCoverageIncomplete, Gaps: []pairCoverageGap{}}
	}
	return w, nil
}

func pairVirtualPublicMutate(root map[string]interface{}) {
	pairTestMemberEntry(root, 0)["source"] = pairSourcePublicSnapshot
}

func pairVirtualSet(market *pairTestMarket, submitLatency, responseLatency int64) *pairVirtualTestSet {
	return &pairVirtualTestSet{build: func() map[string]*pairVirtualVenue {
		out := map[string]*pairVirtualVenue{}
		for _, c := range []pairVirtualVenueConfig{
			{MemberID: pairTestHL, Provider: entryProviderHyperliquid, AccountKey: "synthetic:hl-research", Instrument: "BTC"},
			{MemberID: pairTestOKX, Provider: entryProviderOKX, AccountKey: "synthetic:okx-research", Instrument: "BTC-USDT-SWAP"},
		} {
			c.Partition, c.RunTag, c.Market, c.AdverseBandBps, c.SubmitLatencyMs, c.ResponseLatencyMs = defaultPaperPartition, "run-1", market, 4, submitLatency, responseLatency
			v, err := newPairVirtualVenue(c)
			if err != nil {
				panic(err)
			}
			out[c.MemberID] = v
		}
		return out
	}}
}

func newPairVirtualEnv(t *testing.T, mutate func(root map[string]interface{}), submitLatency, responseLatency int64) (*pairTestEnv, *pairTestMarket) {
	t.Helper()
	market := newPairTestMarket()
	all := func(root map[string]interface{}) {
		pairVirtualPublicMutate(root)
		if mutate != nil {
			mutate(root)
		}
	}
	e := newPairTestEnvWith(t, all, nil, pairVirtualSet(market, submitLatency, responseLatency))
	return e, market
}

func (e *pairTestEnv) vvenue(id string) *pairVirtualVenue {
	return e.virtual.cur[id]
}

func (e *pairTestEnv) member(id string) pairContractMember {
	m, ok := e.rt().Contract.pairMember(id)
	if !ok {
		e.t.Fatalf("no pair member %s", id)
	}
	return m
}

func (e *pairTestEnv) bookObs(id string, venueMs, receiptMs int64, bids, asks [][2]string) pairObservation {
	e.t.Helper()
	m := e.member(id)
	need := e.rt().Contract.Pair.BookNeed
	var payload []byte
	if m.Provider == entryProviderOKX {
		payload = pairFixtureOKXPayload(m.Instrument, need.Depth, venueMs, bids, asks)
	} else {
		payload = pairFixtureHLPublicPayload(m.Instrument, need.Depth, venueMs, bids, asks)
	}
	obs, err := pairObservationFromPayload(m, e.rt().Partition.String(), need, payload, receiptMs, 0)
	if err != nil {
		e.t.Fatalf("book observation for %s: %v", id, err)
	}
	return obs
}

func (e *pairTestEnv) defaultBook(id string, venueMs, receiptMs int64) pairObservation {
	q := e.quotes
	if id == pairTestOKX {
		return e.bookObs(id, venueMs, receiptMs, q.okxBids, q.okxAsks)
	}
	return e.bookObs(id, venueMs, receiptMs, q.hlBids, q.hlAsks)
}

func (e *pairTestEnv) virtualRows(kind string) int {
	return e.count(`SELECT COUNT(*) FROM pair_virtual_events WHERE kind = ?`, kind)
}

func (e *pairTestEnv) outcomeOf(a *basketAction) basketVenueOutcome {
	e.t.Helper()
	var out basketVenueOutcome
	if err := json.Unmarshal([]byte(a.EvidenceJSON), &out); err != nil {
		e.t.Fatalf("action %s evidence: %v", a.ActionID, err)
	}
	return out
}

func pairRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("bad decimal %q", s)
	}
	return r
}

func pairAdverseVWAP(t *testing.T, side string, rate string, cs string, fills [][2]string) (qty, notional float64) {
	t.Helper()
	r := pairRat(t, rate)
	f := new(big.Rat).Add(big.NewRat(1, 1), r)
	if side == "sell" {
		f = new(big.Rat).Sub(big.NewRat(1, 1), r)
	}
	q, n := new(big.Rat), new(big.Rat)
	for _, x := range fills {
		base := new(big.Rat).Mul(pairRat(t, x[1]), pairRat(t, cs))
		q.Add(q, base)
		n.Add(n, new(big.Rat).Mul(base, new(big.Rat).Mul(pairRat(t, x[0]), f)))
	}
	return pairRatFloat(q), pairRatFloat(n)
}

func pairNear(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

func TestBasketPairVirtualVenueFillsBothMembersAtAdverseBoundedPrices(t *testing.T) {
	e, market := newPairVirtualEnv(t, nil, 0, 200)
	pairEnterHoldRunner(e, 0.06)
	at0 := e.slotStep(0)
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 || first[0].StrategyID != pairTestOKX || first[0].Status != basketActionUnresolved {
		t.Fatalf("the first leg must be one accepted, unresolved OKX order, got %+v", first)
	}
	a := first[0]
	s := a.SubmittedAtMs
	if got := e.outcomeOf(a); got.Outcome != pairVenueOpen || got.AcceptedAtMs != s || got.CumQty != 0 {
		t.Fatalf("the submit answer must be open and accepted at the submission time %d, got %+v", s, got)
	}
	if want := fmt.Sprintf("vp-run-1-%s-1", e.vvenue(pairTestOKX).memberHash()); a.ClientOrderID != want {
		t.Fatalf("client id %s, want the deterministic allocation %s", a.ClientOrderID, want)
	}
	market.add(pairTestOKX, e.defaultBook(pairTestOKX, s+500, s+600))
	e.step(time.UnixMilli(s + 700).UTC())
	if a.Status != basketActionUnresolved || a.ConfirmedQty != 0 {
		t.Fatalf("an execution must stay hidden before its visibility time, got %s %.10g", a.Status, a.ConfirmedQty)
	}
	if got := e.outcomeOf(a); got.Outcome != pairVenueOpen || len(got.Fills) != 0 {
		t.Fatalf("before visibility the venue must answer open with no fill totals, got %+v", got)
	}
	if next, ok := e.vvenue(pairTestOKX).NextVisibleMs(); !ok || next != s+800 {
		t.Fatalf("the pending visibility must be max(event, receipt) plus the response latency, got %d %v", next, ok)
	}
	e.step(time.UnixMilli(s + 800).UTC())
	if a.Status != basketActionCommitted || !a.Terminal {
		t.Fatalf("the visible full fill must commit, got %s", a.Status)
	}
	qty, notional := pairAdverseVWAP(t, "sell", "0.0005", "0.01", [][2]string{{"60099.9", "3"}, {"60099.8", "3"}})
	out := e.outcomeOf(a)
	if out.Outcome != basketVenueFilled || !pairNear(out.CumQty, qty) || !pairNear(out.CumNotional, notional) || len(out.Fills) != 2 {
		t.Fatalf("the fill must walk two bid levels at the adverse price, got %+v want %v / %v", out, qty, notional)
	}
	for _, f := range out.Fills {
		if f.EventMs != s+500 || f.VisibleMs != s+800 || f.EventMs < out.AcceptedAtMs || f.VisibleMs > out.ObservedAtMs {
			t.Fatalf("fill timing %+v is not event %d, visible %d inside the answer", f, s+500, s+800)
		}
	}
	tr := e.lastTrade(pairTestOKX)
	if !pairNear(tr.Quantity, qty) || !pairNear(tr.Price, notional/qty) || !pairNear(tr.ExchangeFee, notional*0.00045) {
		t.Fatalf("the booked trade %+v must use the adverse VWAP %v with one modeled taker fee %v and no second slippage", tr, notional/qty, notional*0.00045)
	}
	if tr.CostModelVersion != FillCostModelReplayCustomVersion || len(tr.CostPolicyID) < len(pairCostPolicyIDPrefix) || tr.CostPolicyID[:len(pairCostPolicyIDPrefix)] != pairCostPolicyIDPrefix {
		t.Fatalf("the booked trade must carry the member cost binding, got version %d policy %q", tr.CostModelVersion, tr.CostPolicyID)
	}
	e.assertBooks("first leg", 0, -0.06)
	e.slotStep(1)
	hedge := e.actionsByPurpose(pairPurposeEntry)
	if len(hedge) != 2 || hedge[1].StrategyID != pairTestHL {
		t.Fatalf("the hedge must follow at the next slot, got %+v", hedge)
	}
	h := hedge[1]
	market.add(pairTestHL, e.defaultBook(pairTestHL, h.SubmittedAtMs+100, h.SubmittedAtMs+150))
	e.step(time.UnixMilli(h.SubmittedAtMs + 350).UTC())
	hq, hn := pairAdverseVWAP(t, "buy", "0.0005", "1", [][2]string{{"60001", "0.06"}})
	if hout := e.outcomeOf(h); h.Status != basketActionCommitted || !pairNear(hout.CumQty, hq) || !pairNear(hout.CumNotional, hn) {
		t.Fatalf("the hedge must fill at the adverse ask, got %s %+v", h.Status, hout)
	}
	e.assertBooks("hedged", 0.06, -0.06)
	if e.lifecycle() != pairLifecycleMatched {
		t.Fatalf("the pair must be matched, got %s", e.lifecycle())
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		if n := e.vvenue(id).positionCalls(); n != 0 {
			t.Fatalf("%s virtual venue received %d private position reads", id, n)
		}
	}
	if n := e.virtualRows(pairVirtualKindConsume); n != 3 {
		t.Fatalf("want three saved level consumptions, got %d", n)
	}
	_ = at0
}

var pairVirtualAltBids = [][2]string{{"60090.5", "6"}, {"60090.4", "6"}, {"60090.3", "6"}, {"60090.2", "6"}, {"60090.1", "6"}}

func newPairVirtualFirstLeg(t *testing.T, submitLatency, responseLatency int64) (*pairTestEnv, *pairTestMarket, *basketAction) {
	t.Helper()
	e, market := newPairVirtualEnv(t, nil, submitLatency, responseLatency)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	first := e.actionsByPurpose(pairPurposeEntry)
	if len(first) != 1 || first[0].StrategyID != pairTestOKX {
		t.Fatalf("want one OKX first leg, got %+v", first)
	}
	return e, market, first[0]
}

func (e *pairTestEnv) stepAt(ms int64) {
	e.step(time.UnixMilli(ms).UTC())
}

func (e *pairTestEnv) fillEvent(a *basketAction) (int64, float64) {
	out := e.outcomeOf(a)
	if len(out.Fills) == 0 {
		return 0, 0
	}
	return out.Fills[0].EventMs, out.CumNotional
}

func TestBasketPairVirtualVenueTimingAndAvailabilityOrder(t *testing.T) {
	t.Run("submit latency endpoints", func(t *testing.T) {
		e, market, a := newPairVirtualFirstLeg(t, 300, 0)
		s := a.SubmittedAtMs
		market.add(pairTestOKX, e.defaultBook(pairTestOKX, s+299, s+299), e.bookObs(pairTestOKX, s+300, s+300, pairVirtualAltBids, e.quotes.okxAsks))
		e.stepAt(s + 400)
		if ev, _ := e.fillEvent(a); a.Status != basketActionCommitted || ev != s+300 {
			t.Fatalf("the first eligible book is at submission plus latency %d, got %s at %d", s+300, a.Status, ev)
		}
	})
	t.Run("deadline excluded and snapshots prove no absence", func(t *testing.T) {
		e, market, a := newPairVirtualFirstLeg(t, 0, 0)
		s, deadline := a.SubmittedAtMs, a.AcceptDeadlineMs
		market.add(pairTestOKX, e.defaultBook(pairTestOKX, deadline, deadline))
		e.stepAt(deadline + 10)
		limit := deadline + entryRecoveryGrace.Milliseconds()
		e.stepAt(limit + 1)
		e.stepAt(limit + 60000)
		if a.Status != basketActionUnresolved || a.ConfirmedQty != 0 || e.virtualRows(pairVirtualKindMatch) != 0 || e.virtualRows(pairVirtualKindAbsence) != 0 {
			t.Fatalf("a book at the deadline and point snapshots must leave the order unresolved, got %s, %d matches, %d absences", a.Status, e.virtualRows(pairVirtualKindMatch), e.virtualRows(pairVirtualKindAbsence))
		}
		if got := e.outcomeOf(a); got.Outcome != pairVenueOpen || got.AcceptedAtMs != s {
			t.Fatalf("missing eligible input must stay open at the saved acceptance %d, got %+v", s, got)
		}
	})
	t.Run("first by availability with recorded order ties", func(t *testing.T) {
		results := map[string]float64{}
		for _, firstQuery := range []int64{7, 12} {
			e, market, a := newPairVirtualFirstLeg(t, 0, 0)
			s := a.SubmittedAtMs
			market.add(pairTestOKX,
				e.bookObs(pairTestOKX, s+3, s+10, e.quotes.okxBids, e.quotes.okxAsks),
				e.bookObs(pairTestOKX, s+5, s+6, pairVirtualAltBids, e.quotes.okxAsks),
				e.bookObs(pairTestOKX, s+5, s+6, [][2]string{{"60080", "9"}, {"60079", "9"}, {"60078", "9"}, {"60077", "9"}, {"60076", "9"}}, e.quotes.okxAsks))
			e.stepAt(s + firstQuery)
			if firstQuery == 7 {
				e.restart()
				e.stepAt(s + 12)
				a = e.rt().Actions[a.ActionID]
			}
			ev, n := e.fillEvent(a)
			if a.Status != basketActionCommitted || ev != s+5 {
				t.Fatalf("query at +%d: the earliest available book (event %d) must decide, got %s at %d", firstQuery, s+5, a.Status, ev)
			}
			results[fmt.Sprint(firstQuery)] = n
		}
		_, want := pairAdverseVWAP(t, "sell", "0.0005", "0.01", [][2]string{{"60090.5", "6"}})
		if !pairNear(results["7"], want) || !pairNear(results["12"], want) {
			t.Fatalf("query timing changed the execution: %v, want the first recorded same-time book %v", results, want)
		}
	})
	t.Run("delayed receipt and future mutation", func(t *testing.T) {
		e, market, a := newPairVirtualFirstLeg(t, 0, 50)
		s := a.SubmittedAtMs
		market.add(pairTestOKX, e.defaultBook(pairTestOKX, s+5, s+2000))
		e.stepAt(s + 100)
		if a.Status != basketActionUnresolved || e.virtualRows(pairVirtualKindMatch) != 0 {
			t.Fatalf("a record received after the clock must not execute, got %s", a.Status)
		}
		e.stepAt(s + 2000)
		if a.Status != basketActionUnresolved || e.virtualRows(pairVirtualKindMatch) != 1 {
			t.Fatalf("the delayed record decides at its receipt and stays hidden for the response latency, got %s with %d matches", a.Status, e.virtualRows(pairVirtualKindMatch))
		}
		market.replace(pairTestOKX, e.bookObs(pairTestOKX, s+4, s+1000, pairVirtualAltBids, e.quotes.okxAsks))
		e.restart()
		e.stepAt(s + 2050)
		a = e.rt().Actions[a.ActionID]
		ev, n := e.fillEvent(a)
		_, want := pairAdverseVWAP(t, "sell", "0.0005", "0.01", [][2]string{{"60099.9", "3"}, {"60099.8", "3"}})
		if a.Status != basketActionCommitted || ev != s+5 || !pairNear(n, want) {
			t.Fatalf("changed market input after the saved decision must not change the execution, got %s event %d notional %v", a.Status, ev, n)
		}
	})
}

type pairVirtualRoundTrip struct {
	ids      []string
	trades   []Trade
	fills    [][]pairVenueFill
	consumes int
	matches  int
}

func pairFailCommitOnce(t *testing.T, frame string) *int {
	t.Helper()
	prev := storeCommitHook
	hits := new(int)
	storeCommitHook = func(role storageRole) error {
		if *hits == 0 && strings.Contains(string(debug.Stack()), frame) {
			*hits++
			return fmt.Errorf("injected crash at %s", frame)
		}
		if prev != nil {
			return prev(role)
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	return hits
}

func runPairVirtualRoundTrip(t *testing.T, crash string) pairVirtualRoundTrip {
	t.Helper()
	e, market := newPairVirtualEnv(t, nil, 0, 200)
	pairEnterHoldRunner(e, 0.06)
	var preID string
	if crash == "allocation" {
		e.step(pairTestStart().Add(-30 * time.Second))
		id, err := e.vvenue(pairTestOKX).NewClientOrderID()
		if err != nil {
			t.Fatal(err)
		}
		preID = id
		e.restart()
	}
	var hits *int
	if crash == "acceptance reply" {
		hits = pairFailCommitOnce(t, "applyPairOutcome")
	}
	e.slotStep(0)
	if crash == "acceptance reply" {
		storeCommitHook = nil
		if *hits != 1 {
			t.Fatalf("the acceptance reply crash was not injected")
		}
		e.restart()
	}
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	if preID != "" && a.ClientOrderID != preID {
		t.Fatalf("an allocation saved before reservation must be reused after a restart: %s, want %s", a.ClientOrderID, preID)
	}
	s := a.SubmittedAtMs
	market.add(pairTestOKX, e.defaultBook(pairTestOKX, s+500, s+600))
	e.stepAt(s + 700)
	switch crash {
	case "match reply":
		e.now = time.UnixMilli(s + 800).UTC()
		cp := *a
		if out, err := e.vvenue(pairTestOKX).Query(&cp); err != nil || out.Outcome != basketVenueFilled {
			t.Fatalf("the direct query must commit and publish the match: %+v %v", out, err)
		}
		e.restart()
	case "booking":
		hits = pairFailCommitOnce(t, "settlePairAction")
	}
	e.stepAt(s + 800)
	if crash == "booking" {
		storeCommitHook = nil
		if *hits != 1 {
			t.Fatalf("the booking crash was not injected")
		}
		e.restart()
		e.stepAt(s + 900)
	}
	e.slotStep(1)
	all := e.actionsByPurpose(pairPurposeEntry)
	if len(all) != 2 {
		t.Fatalf("%s: want the first leg and the hedge, got %+v", crash, all)
	}
	h := all[1]
	market.add(pairTestHL, e.defaultBook(pairTestHL, h.SubmittedAtMs+100, h.SubmittedAtMs+150))
	e.stepAt(h.SubmittedAtMs + 350)
	e.assertBooks(crash, 0.06, -0.06)
	var out pairVirtualRoundTrip
	for _, x := range e.actionsByPurpose(pairPurposeEntry) {
		if x.Status != basketActionCommitted {
			t.Fatalf("%s: action %s is %s", crash, x.ActionID, x.Status)
		}
		out.ids = append(out.ids, x.ClientOrderID)
		out.fills = append(out.fills, e.outcomeOf(x).Fills)
	}
	for _, id := range []string{pairTestOKX, pairTestHL} {
		tr := e.lastTrade(id)
		out.trades = append(out.trades, Trade{Quantity: tr.Quantity, Price: tr.Price, ExchangeFee: tr.ExchangeFee, Side: tr.Side})
	}
	out.consumes, out.matches = e.virtualRows(pairVirtualKindConsume), e.virtualRows(pairVirtualKindMatch)
	return out
}

func TestBasketPairVirtualVenueCrashRecoveryMatchesAnUninterruptedRun(t *testing.T) {
	want := runPairVirtualRoundTrip(t, "")
	if want.consumes != 3 || want.matches != 2 {
		t.Fatalf("the uninterrupted run saved %d consumptions and %d matches", want.consumes, want.matches)
	}
	for _, crash := range []string{"allocation", "acceptance reply", "match reply", "booking"} {
		t.Run(crash, func(t *testing.T) {
			got := runPairVirtualRoundTrip(t, crash)
			if fmt.Sprint(got.ids) != fmt.Sprint(want.ids) || fmt.Sprint(got.fills) != fmt.Sprint(want.fills) || fmt.Sprint(got.trades) != fmt.Sprint(want.trades) || got.consumes != want.consumes || got.matches != want.matches {
				t.Fatalf("a crash at %s changed the execution:\n got  %+v\n want %+v", crash, got, want)
			}
		})
	}
}

func TestBasketPairVirtualVenueSpentIdentifierIsNeverReused(t *testing.T) {
	e, market := newPairVirtualEnv(t, nil, 0, 0)
	pairEnterHoldRunner(e, 0.06)
	hits := pairFailCommitOnce(t, "markPairActionSubmittingTx")
	e.slotStep(0)
	storeCommitHook = nil
	first := e.actionsByPurpose(pairPurposeEntry)
	if *hits != 1 || len(first) != 1 || first[0].Status != basketActionNoFill {
		t.Fatalf("a failed submitting mark must end the reserved first leg unsent, got %d hits and %+v", *hits, first)
	}
	if n := e.virtualRows(pairVirtualKindSubmit); n != 0 {
		t.Fatalf("the venue accepted %d orders although nothing was sent", n)
	}
	e.restart()
	e.slotStep(1)
	all := e.actionsByPurpose(pairPurposeEntry)
	if len(all) != 2 {
		t.Fatalf("the next slot must dispatch a new first leg, got %+v", all)
	}
	hash := e.vvenue(pairTestOKX).memberHash()
	if all[0].ClientOrderID != "vp-run-1-"+hash+"-1" || all[1].ClientOrderID != "vp-run-1-"+hash+"-2" {
		t.Fatalf("a bound identifier is spent even when nothing was sent: %s then %s", all[0].ClientOrderID, all[1].ClientOrderID)
	}
	market.add(pairTestOKX, e.defaultBook(pairTestOKX, all[1].SubmittedAtMs+10, all[1].SubmittedAtMs+10))
	e.stepAt(all[1].SubmittedAtMs + 20)
	if e.rt().Actions[all[1].ActionID].Status != basketActionCommitted {
		t.Fatalf("the new identifier must execute, got %s", e.rt().Actions[all[1].ActionID].Status)
	}
	if n := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`); n != 2 {
		t.Fatalf("want two allocations, got %d", n)
	}
	if _, err := e.db().db.Exec(`INSERT INTO pair_virtual_allocations (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument, client_order_id, created_at_ms)
		VALUES ('run-1', '{"other":"member"}', 1, 'paper', '', ?, 'x', 'okx', 'synthetic:x', 'X', ?, 1)`, pairTestID, "vp-run-1-"+hash+"-3"); err != nil {
		t.Fatal(err)
	}
	before := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`)
	if _, err := e.vvenue(pairTestOKX).NewClientOrderID(); err == nil {
		t.Fatalf("a truncated-hash collision with another member's allocation must refuse")
	}
	if after := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`); after != before {
		t.Fatalf("a refused allocation changed the saved allocations (%d -> %d)", before, after)
	}
}

func (e *pairTestEnv) reserveVirtual(id, purpose, orderSide, positionSide string, base, native float64, deadlineMs int64) *basketAction {
	e.t.Helper()
	e.bc.beginCycle()
	e.observe("")
	e.prepare(e.now)
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt()
	rt.clock = e.now
	m := e.member(id)
	now := e.now.UnixMilli()
	ref, why := e.bc.pairReductionReference(rt, m, orderSide, now)
	if why != "" {
		e.t.Fatalf("reduction reference: %s", why)
	}
	bound, why := pairReductionBound(m, orderSide, ref.Ref, now, rt.Pair.Limits)
	if why != "" {
		e.t.Fatalf("reduction bound: %s", why)
	}
	cloid, err := e.vvenue(id).NewClientOrderID()
	if err != nil {
		e.t.Fatalf("allocate: %v", err)
	}
	slot := now
	if rb := rt.latest(); rb != nil {
		slot = rb.DeadlineMs
	}
	a := &basketAction{GroupMode: pairGroupMode, Partition: rt.Partition, BasketID: rt.Contract.BasketID, DeadlineMs: slot, StrategyID: id, Symbol: m.Symbol,
		Direction: basketDirectionReduce, OrderSide: orderSide, PositionSide: positionSide, Status: basketActionReserved, RequestedQty: base, Provider: m.Provider,
		AccountKey: m.AccountKey, ClientOrderID: cloid, Purpose: purpose, Instrument: m.Instrument, NativeQty: native, ContractSize: m.Meta.ContractSize,
		PriceBound: bound, AcceptDeadlineMs: deadlineMs, CreatedAtMs: now, UpdatedAtMs: now}
	a.RequestJSON = marshalEntryJSON(pairActionRequest{V: pairActionRequestVersion, PairID: a.BasketID, Partition: rt.Partition.String(), MemberID: id, Provider: m.Provider,
		AccountKey: m.AccountKey, Instrument: m.Instrument, ClientOrderID: cloid, Purpose: purpose, OrderSide: orderSide, PositionSide: positionSide, BaseQty: base,
		NativeQty: native, ContractSize: m.Meta.ContractSize, PriceBound: bound, AcceptDeadlineMs: deadlineMs, OriginSlotMs: slot, ReduceOnly: true, ContractFingerprint: rt.Contract.Fingerprint(),
		Reference: &ref.Ref, ReferenceMaxAgeMs: rt.Pair.Limits.QuoteAgeMs, ReferenceReceiptMs: ref.Ref.FirstReceiptMs, ReferencePx: ref.Px})
	if err := e.store.ReservePairAction(rt, a, nil, nil); err != nil {
		e.t.Fatalf("reserve: %v", err)
	}
	if err := e.store.MarkPairActionSubmitting(rt, a, nil); err != nil {
		e.t.Fatalf("mark submitting: %v", err)
	}
	e.vvenue(id).noteNativeInventory(e.bc.pairNativeRows(rt, id))
	return a
}

func (e *pairTestEnv) virtualFirstLegFilled(market *pairTestMarket) *basketAction {
	e.t.Helper()
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	market.add(pairTestOKX, e.defaultBook(pairTestOKX, a.SubmittedAtMs+10, a.SubmittedAtMs+10))
	e.stepAt(a.SubmittedAtMs + 20)
	if a.Status != basketActionCommitted {
		e.t.Fatalf("the first leg must fill, got %s", a.Status)
	}
	return a
}

func TestBasketPairVirtualVenueRefusesWithoutConsumingAndHoldsProtection(t *testing.T) {
	e, market := newPairVirtualEnv(t, nil, 0, 0)
	first := e.virtualFirstLegFilled(market)
	okx := e.vvenue(pairTestOKX)
	t.Run("changed request under a saved identity", func(t *testing.T) {
		rows := e.count(`SELECT COUNT(*) FROM pair_virtual_events`)
		cp := *first
		cp.RequestJSON = first.RequestJSON[:len(first.RequestJSON)-1] + `,"x":1}`
		out, err := okx.Query(&cp)
		if err != nil || out.Outcome != basketVenueUnknown || e.count(`SELECT COUNT(*) FROM pair_virtual_events`) != rows {
			t.Fatalf("a changed request must answer unknown and write nothing, got %+v %v", out, err)
		}
	})
	t.Run("protective request whose saved reference is stale at acceptance", func(t *testing.T) {
		e.now = e.now.Add(time.Second)
		a := e.reserveVirtual(pairTestOKX, pairPurposeProtective, "buy", entrySideShort, 0.06, 6, e.now.UnixMilli()+30000)
		e.now = e.now.Add(time.Duration(e.rt().Pair.Limits.QuoteAgeMs+1000) * time.Millisecond)
		market.add(pairTestOKX, e.defaultBook(pairTestOKX, e.now.UnixMilli()+5, e.now.UnixMilli()+5))
		consumed := e.virtualRows(pairVirtualKindConsume)
		out, err := okx.Submit(a)
		if err != nil || out.Outcome != pairVenueNeverSent || !strings.Contains(out.Detail, "reference") || e.virtualRows(pairVirtualKindRefusal) != 1 || e.virtualRows(pairVirtualKindSubmit) != 1 {
			t.Fatalf("a reduction whose saved reference is older than quote_age_ms at acceptance must be a durable never-sent refusal, got %+v %v", out, err)
		}
		e.now = e.now.Add(time.Second)
		again, err := okx.Query(a)
		if err != nil || again.Outcome != pairVenueNeverSent || e.virtualRows(pairVirtualKindConsume) != consumed {
			t.Fatalf("a query of a refused client must stay never sent with no consumption, got %+v %v", again, err)
		}
	})
	t.Run("never accepted identity", func(t *testing.T) {
		id, err := okx.NewClientOrderID()
		if err != nil {
			t.Fatal(err)
		}
		deadline := e.now.UnixMilli() + 1000
		ghost := &basketAction{Partition: first.Partition, StrategyID: pairTestOKX, Provider: first.Provider, AccountKey: first.AccountKey, Instrument: first.Instrument,
			ClientOrderID: id, AcceptDeadlineMs: deadline, RequestJSON: "{}"}
		if out, err := okx.Query(ghost); err != nil || out.Outcome != basketVenueUnknown || e.virtualRows(pairVirtualKindDeadline) != 0 {
			t.Fatalf("before the deadline an unaccepted client must be unknown, got %+v %v", out, err)
		}
		e.now = time.UnixMilli(deadline).UTC()
		if out, err := okx.Query(ghost); err != nil || out.Outcome != pairVenueNeverSent || e.virtualRows(pairVirtualKindDeadline) != 1 {
			t.Fatalf("at the deadline the venue must save the deadline record and answer never sent, got %+v %v", out, err)
		}
		if out, err := okx.Submit(ghost); err != nil || out.Outcome != pairVenueNeverSent || e.virtualRows(pairVirtualKindSubmit) != 1 {
			t.Fatalf("a later submission of a deadline-recorded client must stay never sent, got %+v %v", out, err)
		}
	})
	t.Run("off-lot reduction that is not the full exposure", func(t *testing.T) {
		e.now = e.now.Add(time.Second)
		a := e.reserveVirtual(pairTestOKX, pairPurposeExit, "buy", entrySideShort, 0.055, 5.5, e.now.UnixMilli()+30000)
		out, err := okx.Submit(a)
		if err != nil || out.Outcome != pairVenueNeverSent || e.virtualRows(pairVirtualKindRefusal) != 2 || e.virtualRows(pairVirtualKindSubmit) != 1 {
			t.Fatalf("an off-lot reduction below the known exposure must refuse durably, got %+v %v", out, err)
		}
	})
	t.Run("virtual protective reductions use the saved reference width bound", func(t *testing.T) {
		e2, market2 := newPairVirtualEnv(t, nil, 0, 0)
		e2.virtualFirstLegFilled(market2)
		e2.externalBookChange(pairTestHL, 0.01)
		at := e2.now.UnixMilli() + 1000
		e2.stepAt(at)
		if why := e2.bc.pairIncreaseHold(e2.rt(), e2.inputs(e2.now)); why == "" {
			t.Fatalf("recovery and the unexplained book change must hold increases")
		}
		for i := 0; i < 4 && (e2.signed(pairTestHL) != 0 || e2.signed(pairTestOKX) != 0); i++ {
			for _, x := range e2.actionsByPurpose(pairPurposeProtective) {
				if basketActionPending(x.Status) {
					market2.add(x.StrategyID, e2.defaultBook(x.StrategyID, x.SubmittedAtMs+10, x.SubmittedAtMs+10))
				}
			}
			at += 100
			e2.stepAt(at)
		}
		e2.assertBooks("virtual protective", 0, 0)
		p := e2.rt().Pair.Progress
		if p.obligation(pairObligationProtectiveBound, pairTestOKX, "") != nil || p.obligation(pairObligationProtectiveBound, pairTestHL, "") != nil ||
			p.obligation(pairObligationReductionReference, pairTestOKX, "") != nil || p.obligation(pairObligationReductionReference, pairTestHL, "") != nil {
			t.Fatalf("a virtual protective reduction with a saved reference must not hold, got %+v", p.Obligations)
		}
		src := e2.submitBoundSources()
		prot := e2.actionsByPurpose(pairPurposeProtective)
		members := map[string]bool{}
		for _, x := range prot {
			if x.Status != basketActionCommitted {
				continue
			}
			members[x.StrategyID] = true
			var req pairActionRequest
			if err := json.Unmarshal([]byte(x.RequestJSON), &req); err != nil || req.Reference == nil {
				t.Fatalf("protective %s must save its reduction reference: %v", x.ActionID, err)
			}
			want, why := pairReductionBound(e2.member(x.StrategyID), req.OrderSide, *req.Reference, req.ReferenceReceiptMs, e2.rt().Pair.Limits)
			if why != "" || want != x.PriceBound || req.PriceBound != x.PriceBound {
				t.Fatalf("protective %s bound %v must be the width bound %v of its saved reference (%s)", x.ActionID, x.PriceBound, want, why)
			}
			if src[x.ClientOrderID] != pairVirtualBoundReference {
				t.Fatalf("protective %s must fill with the saved reference bound, got %q", x.ClientOrderID, src[x.ClientOrderID])
			}
		}
		if !members[pairTestOKX] || !members[pairTestHL] {
			t.Fatalf("both exposed members must reduce through the virtual venue, got %+v", prot)
		}
	})
}

func (e *pairTestEnv) submitBoundSources() map[string]string {
	e.t.Helper()
	rows, err := e.db().db.Query(`SELECT client_order_id, body_json FROM pair_virtual_events WHERE kind = 'submit' ORDER BY client_order_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			e.t.Fatal(err)
		}
		var b pairVirtualSubmitBody
		if err := pairVirtualDecodeBody(raw, &b); err != nil {
			e.t.Fatal(err)
		}
		out[id] = b.BoundSource
	}
	return out
}

func TestBasketPairVirtualVenueHoldAgeExitUsesTheSavedReductionBound(t *testing.T) {
	e, market := newPairVirtualEnv(t, pairSetLimit("max_hold_age_ms", 150000), 0, 0)
	e.virtualFirstLegFilled(market)
	e.slotStep(1)
	h := e.actionsByPurpose(pairPurposeEntry)[1]
	market.add(pairTestHL, e.defaultBook(pairTestHL, h.SubmittedAtMs+10, h.SubmittedAtMs+10))
	e.stepAt(h.SubmittedAtMs + 20)
	if e.lifecycle() != pairLifecycleMatched {
		t.Fatalf("want a matched pair, got %s", e.lifecycle())
	}
	end := e.rt().Pair.Progress.FirstFillMs + 150000
	for i := 0; i < 4 && (e.signed(pairTestHL) != 0 || e.signed(pairTestOKX) != 0); i++ {
		e.stepAt(end + int64(i)*100)
		for _, x := range e.actionsByPurpose(pairPurposeExit) {
			if !basketActionPending(x.Status) {
				continue
			}
			if x.StrategyID == pairTestOKX {
				market.add(x.StrategyID, e.bookObs(x.StrategyID, x.SubmittedAtMs+10, x.SubmittedAtMs+10,
					[][2]string{{"59999.9", "9"}, {"59999.8", "9"}, {"59999.7", "9"}, {"59999.6", "9"}, {"59999.5", "9"}},
					[][2]string{{"60000", "9"}, {"60000.1", "9"}, {"60000.2", "9"}, {"60000.3", "9"}, {"60000.4", "9"}}))
			} else {
				market.add(x.StrategyID, e.bookObs(x.StrategyID, x.SubmittedAtMs+10, x.SubmittedAtMs+10,
					[][2]string{{"60100", "1"}, {"60099", "1"}, {"60098", "1"}, {"60097", "1"}, {"60096", "1"}},
					[][2]string{{"60101", "1"}, {"60102", "1"}, {"60103", "1"}, {"60104", "1"}, {"60105", "1"}}))
			}
		}
		e.stepAt(end + int64(i)*100 + 50)
	}
	e.assertBooks("hold-age exit", 0, 0)
	src := e.submitBoundSources()
	rows, err := e.db().db.Query(`SELECT client_order_id, strategy_id, request_json, price_bound FROM basket_member_actions WHERE purpose = 'pair_exit' AND status = 'committed' ORDER BY client_order_id`)
	if err != nil {
		t.Fatal(err)
	}
	var exits []basketAction
	for rows.Next() {
		var x basketAction
		if err := rows.Scan(&x.ClientOrderID, &x.StrategyID, &x.RequestJSON, &x.PriceBound); err != nil {
			t.Fatal(err)
		}
		x.ActionID = x.ClientOrderID
		exits = append(exits, x)
	}
	rows.Close()
	committed := 0
	for _, x := range exits {
		committed++
		var req pairActionRequest
		if err := json.Unmarshal([]byte(x.RequestJSON), &req); err != nil || req.Reference == nil {
			t.Fatalf("hold-age exit %s must save its reduction reference: %v", x.ActionID, err)
		}
		want, why := pairReductionBound(e.member(x.StrategyID), req.OrderSide, *req.Reference, req.ReferenceReceiptMs, e.rt().Pair.Limits)
		if why != "" || want != x.PriceBound || req.PriceBound != x.PriceBound {
			t.Fatalf("hold-age exit %s bound %v must be the width bound %v of its saved reference (%s)", x.ActionID, x.PriceBound, want, why)
		}
		if src[x.ClientOrderID] != pairVirtualBoundReference {
			t.Fatalf("a hold-age exit without exit decision evidence must fill with its saved reference bound, got %q for %s", src[x.ClientOrderID], x.ClientOrderID)
		}
	}
	if committed != 2 {
		t.Fatalf("want two committed exit tranches, got %d", committed)
	}
	for _, x := range e.actionsByPurpose(pairPurposeEntry) {
		if src[x.ClientOrderID] != pairVirtualBoundEvidence {
			t.Fatalf("entry %s must use the saved decision evidence, got %q", x.ClientOrderID, src[x.ClientOrderID])
		}
	}
}

func (e *pairTestEnv) virtualMatched(market *pairTestMarket) {
	e.t.Helper()
	e.virtualFirstLegFilled(market)
	e.slotStep(1)
	h := e.actionsByPurpose(pairPurposeEntry)[1]
	market.add(pairTestHL, e.defaultBook(pairTestHL, h.SubmittedAtMs+10, h.SubmittedAtMs+10))
	e.stepAt(h.SubmittedAtMs + 20)
	e.assertBooks("matched", 0.06, -0.06)
}

func (e *pairTestEnv) inventoryOrigins(id string) map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string]int{}
	for _, r := range e.rt().Pair.funding.members[id].rows {
		out[r.Origin]++
	}
	return out
}

func (e *pairTestEnv) saveBooks() {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		e.t.Fatalf("save the member books: %v", err)
	}
}

func (e *pairTestEnv) conservation() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bc.pairVirtualConservation(e.rt())
}

func TestBasketPairVirtualVenueNativeBookChangesCountOnce(t *testing.T) {
	e, market := newPairVirtualEnv(t, nil, 0, 0)
	e.virtualMatched(market)
	if why := e.conservation(); why != "" {
		t.Fatalf("matched virtual executions must conserve inventory: %s", why)
	}
	e.mu.Lock()
	e.state.Strategies[pairTestHL].Positions["BTC"].StopLossTriggerPx = 60500
	e.mu.Unlock()
	logger := e.bc.memberLogger(pairTestHL)
	if n, _ := applyPaperStopLossBreach(e.bc.members[pairTestHL], e.state.Strategies[pairTestHL], "BTC", entrySideLong, 60000, e.mu, logger); n != 1 {
		t.Fatalf("the native paper stop must close the long member book")
	}
	e.stepAt(e.now.UnixMilli() + 1000)
	e.saveBooks()
	if got := e.inventoryOrigins(pairTestHL); got[pairInventoryOriginNative] != 1 || got[pairInventoryOriginUnexplained] != 0 {
		t.Fatalf("the paper stop must enter the inventory journal once as a native change, got %v", got)
	}
	if why := e.conservation(); why != "" {
		t.Fatalf("a native paper stop counted once must conserve inventory: %s", why)
	}
	e.restart()
	if why := e.conservation(); why != "" || e.inventoryOrigins(pairTestHL)[pairInventoryOriginNative] != 1 {
		t.Fatalf("after a restart the native change must still count once: %q %v", why, e.inventoryOrigins(pairTestHL))
	}
	e.mu.Lock()
	closed := forceClosePaperScopePositions(e.state, e.cfg, e.rt().Partition, map[string]float64{"BTC": 60000})
	e.mu.Unlock()
	if len(closed) != 0 || e.signed(pairTestOKX) != -0.06 {
		t.Fatalf("a shared-price emergency close must leave the short member book for its own checked mark, got %v %v", closed, e.signed(pairTestOKX))
	}
	e.stepAt(e.now.UnixMilli() + 1000)
	e.mu.RLock()
	view := e.rt().pairView[pairTestOKX]
	e.mu.RUnlock()
	own, valid, why := view.usdMarkAt(e.now.UnixMilli())
	tr, ok := e.closeTrade(pairTestOKX)
	if e.signed(pairTestOKX) != 0 || !valid || !ok || !pairCostNear(tr.Price, own) || !strings.Contains(tr.Details, "own checked mark") {
		t.Fatalf("the emergency paper close must flatten the short member at its own checked mark %v (%s), got book %v trade %+v", own, why, e.signed(pairTestOKX), tr)
	}
	if got := e.inventoryOrigins(pairTestOKX); got[pairInventoryOriginNative] != 1 {
		t.Fatalf("the emergency close must enter the journal once as a native change, got %v", got)
	}
	if why := e.conservation(); why != "" {
		t.Fatalf("the emergency close counted once must conserve inventory: %s", why)
	}
	e.externalBookChange(pairTestOKX, -0.02)
	if why := e.conservation(); why == "" {
		t.Fatalf("an unexplained book change must break virtual inventory conservation")
	}
	if why := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now)); why == "" {
		t.Fatalf("an unexplained book change must hold increases")
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		if n := e.vvenue(id).positionCalls(); n != 0 {
			t.Fatalf("%s virtual venue received %d position reads", id, n)
		}
	}
}

func TestBasketPairVirtualVenueConservationHoldsIncreases(t *testing.T) {
	e, market := newPairVirtualEnv(t, nil, 0, 0)
	e.virtualMatched(market)
	e.externalBookChange(pairTestOKX, -0.07)
	why := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
	if !strings.HasPrefix(why, "virtual inventory conservation") {
		t.Fatalf("an unexplained change must hold increases through the conservation check, got %q", why)
	}
	if e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("the conservation hold must apply before recovery freezes increases")
	}
}

func (e *pairTestEnv) virtualFootprint() string {
	return fmt.Sprintf("%d/%d/%s", e.count(`SELECT COUNT(*) FROM pair_virtual_events`), e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`),
		func() string {
			var h string
			_ = e.db().db.QueryRow(`SELECT COALESCE(group_concat(event_hash, ','), '') FROM (SELECT event_hash FROM pair_virtual_events ORDER BY run_tag, member_key_json, seq)`).Scan(&h)
			return h
		}())
}

func TestBasketPairVirtualVenueAttachmentRefusesChangedBindings(t *testing.T) {
	base := pairVirtualVenueConfig{Partition: defaultPaperPartition, RunTag: "run-1", MemberID: pairTestOKX, Provider: entryProviderOKX, AccountKey: "synthetic:okx-research",
		Instrument: "BTC-USDT-SWAP", Market: newPairTestMarket(), AdverseBandBps: 4}
	bad := map[string]func(c *pairVirtualVenueConfig){
		"run tag":          func(c *pairVirtualVenueConfig) { c.RunTag = "Run 1" },
		"live partition":   func(c *pairVirtualVenueConfig) { c.Partition = RiskPartition{Scope: ScopeLive} },
		"real account":     func(c *pairVirtualVenueConfig) { c.AccountKey = "okx:swap:live:key-0123456789abcdef" },
		"negative latency": func(c *pairVirtualVenueConfig) { c.SubmitLatencyMs = -1 },
		"no market":        func(c *pairVirtualVenueConfig) { c.Market = nil },
		"adverse band":     func(c *pairVirtualVenueConfig) { c.AdverseBandBps = 10000 },
	}
	for name, mutate := range bad {
		c := base
		mutate(&c)
		if v, err := newPairVirtualVenue(c); err == nil || v != nil {
			t.Fatalf("%s: the constructor must refuse, got %v", name, err)
		}
	}
	e, market := newPairVirtualEnv(t, nil, 0, 0)
	first := e.virtualFirstLegFilled(market)
	before := e.virtualFootprint()
	fresh := e.virtual.build()[pairTestOKX]
	if _, err := fresh.NewClientOrderID(); err == nil {
		t.Fatalf("an unattached venue must not allocate")
	}
	if _, err := fresh.Submit(first); err == nil {
		t.Fatalf("an unattached venue must not accept")
	}
	if _, err := fresh.Query(first); err == nil {
		t.Fatalf("an unattached venue must not answer queries")
	}
	clock := func() time.Time { return e.now }
	attach := func(v *pairVirtualVenue, sc StrategyConfig) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		return v.attach(pairVirtualAttachment{Store: e.store, Runtime: e.rt(), Config: sc, Clock: clock})
	}
	okxCfg, hlCfg := e.bc.members[pairTestOKX], e.bc.members[pairTestHL]
	if err := attach(e.vvenue(pairTestOKX), okxCfg); err == nil {
		t.Fatalf("a second attachment must refuse")
	}
	unclocked := e.virtual.build()
	savedClock, savedVenues := e.bc.pairClock, e.bc.venueFor
	e.bc.venueFor = func(sc StrategyConfig) (basketVenue, error) { return unclocked[sc.ID], nil }
	e.bc.pairClock = nil
	err := e.bc.attachPairVirtualVenues()
	e.bc.pairClock, e.bc.venueFor = savedClock, savedVenues
	if err == nil || !strings.Contains(err.Error(), "no pair clock") || unclocked[pairTestOKX].isAttached() || unclocked[pairTestHL].isAttached() {
		t.Fatalf("attachment with no coordinator clock must refuse and attach nothing: %v", err)
	}
	if after := e.virtualFootprint(); after != before {
		t.Fatalf("a refused clockless attachment changed the saved records: %s -> %s", before, after)
	}
	replaced := time.UnixMilli(e.now.UnixMilli() + 1234).UTC()
	e.bc.pairClock = func() time.Time { return replaced }
	got := e.vvenue(pairTestOKX).clock()
	e.bc.pairClock = savedClock
	if !got.Equal(replaced) {
		t.Fatalf("the attached venue must read the current coordinator clock, got %v want %v", got, replaced)
	}
	cases := map[string]func(c *pairVirtualVenueConfig) StrategyConfig{
		"resumed latency": func(c *pairVirtualVenueConfig) StrategyConfig { c.ResponseLatencyMs = 500; return okxCfg },
		"resumed band":    func(c *pairVirtualVenueConfig) StrategyConfig { c.AdverseBandBps = 7; return okxCfg },
		"run identity":    func(c *pairVirtualVenueConfig) StrategyConfig { c.RunTag = "run-2"; return okxCfg },
		"member":          func(c *pairVirtualVenueConfig) StrategyConfig { return hlCfg },
		"partition": func(c *pairVirtualVenueConfig) StrategyConfig {
			c.Partition = RiskPartition{Scope: ScopePaper, Source: "other"}
			return okxCfg
		},
		"instrument owner": func(c *pairVirtualVenueConfig) StrategyConfig { c.Instrument = "ETH-USDT-SWAP"; return okxCfg },
	}
	for name, mutate := range cases {
		c := base
		c.Market = market
		sc := mutate(&c)
		v, err := newPairVirtualVenue(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := attach(v, sc); err == nil {
			t.Fatalf("%s: a changed attachment must refuse", name)
		}
		if _, err := v.NewClientOrderID(); err == nil {
			t.Fatalf("%s: a refused attachment must leave the venue unusable", name)
		}
	}
	if after := e.virtualFootprint(); after != before {
		t.Fatalf("refused attachments changed the saved execution records: %s -> %s", before, after)
	}
}

func TestBasketPairVirtualVenueStorageOwnershipAndInspection(t *testing.T) {
	t.Run("old files migrate twice and inspect read-only", func(t *testing.T) {
		path := pairOldSchemaFile(t)
		ro, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		fi := &storageFileInspection{Role: storageRolePrimary}
		if rej, err := inspectPairVirtualRows(ro, fi, storageIdentityMap{}, map[PortfolioScope]bool{ScopePaper: true}, nil); err != nil || len(rej) != 0 || fi.PairVirtualEvents != 0 {
			t.Fatalf("inspection of a file without the virtual tables must report nothing: %v %v", rej, err)
		}
		ro.Close()
		for run := 1; run <= 2; run++ {
			db, err := OpenStateDB(path)
			if err != nil {
				t.Fatalf("migration run %d: %v", run, err)
			}
			for _, table := range []string{"pair_virtual_events", "pair_virtual_allocations"} {
				var n int
				if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("run %d: %s has %d rows (%v)", run, table, n, err)
				}
			}
			db.Close()
		}
	})
	t.Run("alias owner, inspection and corruption", func(t *testing.T) {
		e, market := newPairVirtualEnv(t, func(root map[string]interface{}) {
			pairTestStrategy(root, pairTestOKX)["storage_strategy_id"] = "okx-store"
		}, 0, 0)
		e.virtualFirstLegFilled(market)
		var sid, key string
		if err := e.db().db.QueryRow(`SELECT strategy_id, member_key_json FROM pair_virtual_allocations WHERE provider = 'okx'`).Scan(&sid, &key); err != nil {
			t.Fatal(err)
		}
		if sid != "okx-store" || !strings.Contains(key, `"strategy_id":"okx-store"`) {
			t.Fatalf("virtual records must store the storage alias, got %s %s", sid, key)
		}
		e.restart()
		if why := e.conservation(); why != "" {
			t.Fatalf("an aliased member must reload its virtual history: %s", why)
		}
		path := filepath.Join(e.dir, "state.db")
		ident, layout := pairTestIdentity(t, e.cfg)
		ro, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		fi := &storageFileInspection{Role: storageRolePrimary, ScopesOwned: layout.scopesForRole(storageRolePrimary), PartitionsOwned: partitionTextList(layout.partitionsForRole(storageRolePrimary))}
		owned := map[PortfolioScope]bool{}
		for _, s := range fi.ScopesOwned {
			owned[s] = true
		}
		rej, err := inspectPairVirtualRows(ro, fi, ident, owned, nil)
		ro.Close()
		if err != nil || len(rej) != 0 || fi.PairVirtualEvents == 0 || fi.PairVirtualAllocations != 1 {
			t.Fatalf("inspection must map the aliased owner with no rejection: %v %v %+v", rej, err, fi)
		}
		var last string
		var seq int
		if err := e.db().db.QueryRow(`SELECT member_key_json, MAX(seq) FROM pair_virtual_events WHERE provider = 'okx'`).Scan(&last, &seq); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(`INSERT INTO pair_virtual_events (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument, kind, semantic_key, body_json, previous_event_hash, event_hash, created_at_ms)
			VALUES ('run-1', ?, ?, 'paper', '', ?, 'okx-store', 'okx', 'synthetic:okx-research', 'BTC-USDT-SWAP', 'query', 'forged', '{}', 'bad', 'bad', 1)`, last, seq+1, pairTestID); err != nil {
			t.Fatal(err)
		}
		ro, err = openStateDBReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		fi = &storageFileInspection{Role: storageRolePrimary, ScopesOwned: fi.ScopesOwned, PartitionsOwned: fi.PartitionsOwned}
		rej, err = inspectPairVirtualRows(ro, fi, ident, owned, nil)
		ro.Close()
		if err != nil || len(rej) != 1 || !strings.Contains(rej[0], "is corrupt") {
			t.Fatalf("a broken hash chain must be reported as corrupt: %v %v", rej, err)
		}
		if _, err := e.store.LoadPairVirtualLedger(e.rt(), e.vvenue(pairTestOKX).id); err == nil {
			t.Fatalf("a broken hash chain must refuse to load")
		}
	})
	t.Run("long history appends from the verified tip", func(t *testing.T) {
		e, market := newPairVirtualEnv(t, nil, 0, 0)
		okx := e.vvenue(pairTestOKX)
		id, binding := okx.id, okx.binding
		_, key, err := e.db().pairVirtualKey(id)
		if err != nil {
			t.Fatal(err)
		}
		appendQueries := func(from, n int) ([]pairVirtualEvent, error) {
			evs := make([]pairVirtualEvent, 0, n)
			for i := from; i < from+n; i++ {
				evs = append(evs, pairVirtualEvent{Kind: pairVirtualKindQuery, SemanticKey: fmt.Sprintf("query|long|%d", i), Body: `{}`})
			}
			return e.store.AppendPairVirtualEvents(e.rt(), id, binding, evs, nil, e.now.UnixMilli())
		}
		for b := 0; b < 10; b++ {
			if _, err := appendQueries(b*1000, 1000); err != nil {
				t.Fatal(err)
			}
		}
		chainHash := func(seq int) string {
			var h string
			if err := e.db().db.QueryRow(`SELECT event_hash FROM pair_virtual_events WHERE run_tag = 'run-1' AND member_key_json = ? AND seq = ?`, key, seq).Scan(&h); err != nil {
				t.Fatal(err)
			}
			return h
		}
		if tip := e.db().pairVirtualVerifiedTip("run-1", key); tip.Seq != 10001 || tip.Hash != chainHash(10001) {
			t.Fatalf("10,000 saved events must leave the verified tip at seq 10001, got %+v", tip)
		}
		e.db().notePairVirtualTip("run-1", key, pairVirtualTip{Seq: 5000, Hash: chainHash(5000)})
		saved, err := appendQueries(20000, 1)
		if err != nil || len(saved) != 1 || saved[0].Seq != 10002 || saved[0].PrevHash != chainHash(10001) {
			t.Fatalf("an append after rows saved beyond the verified tip must chain onto the newest row with no fork: %+v %v", saved, err)
		}
		if _, err := e.store.LoadPairVirtualLedger(e.rt(), id); err != nil {
			t.Fatalf("the chain extended past a lagging tip must stay whole: %v", err)
		}
		e.db().notePairVirtualTip("run-1", key, pairVirtualTip{Seq: 9000, Hash: chainHash(9000)})
		if _, err := e.db().db.Exec(`INSERT INTO pair_virtual_events (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument, kind, semantic_key, body_json, previous_event_hash, event_hash, created_at_ms)
			VALUES ('run-1', ?, 10003, 'paper', '', ?, ?, 'okx', 'synthetic:okx-research', 'BTC-USDT-SWAP', 'query', 'forged-tail', '{}', ?, 'bad', 1)`, key, pairTestID, pairTestOKX, chainHash(10002)); err != nil {
			t.Fatal(err)
		}
		before := e.virtualFootprint()
		if _, err := appendQueries(20001, 1); err == nil || !strings.Contains(err.Error(), "does not match its content") {
			t.Fatalf("a forged row after the verified tip must refuse the append: %v", err)
		}
		if after := e.virtualFootprint(); after != before {
			t.Fatalf("a refused append changed the saved records")
		}
		if _, err := e.db().db.Exec(`DROP TRIGGER trg_pair_virtual_events_kept`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(`DELETE FROM pair_virtual_events WHERE run_tag = 'run-1' AND member_key_json = ? AND seq = 10003`, key); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(`DROP TRIGGER trg_pair_virtual_events_immutable`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(`UPDATE pair_virtual_events SET body_json = '{"forged":true}' WHERE run_tag = 'run-1' AND member_key_json = ? AND seq = 5000`, key); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(pairVirtualDDL); err != nil {
			t.Fatal(err)
		}
		e.db().notePairVirtualTip("run-1", key, pairVirtualTip{Seq: 10002, Hash: chainHash(10002)})
		first := e.virtualFirstLegFilled(market)
		if n := e.count(`SELECT COUNT(*) FROM pair_virtual_events WHERE run_tag = 'run-1' AND member_key_json = ? AND client_order_id = ? AND seq > 10002`, key, first.ClientOrderID); n < 2 {
			t.Fatalf("allocation, submit, query and match over 10,002 saved events must append from the verified tip and never re-read the older history, got %d records", n)
		}
		if _, err := e.store.LoadPairVirtualLedger(e.rt(), id); err == nil || !strings.Contains(err.Error(), "event 5000 hash") {
			t.Fatalf("a full load must refuse a tampered middle row: %v", err)
		}
		c := pairVirtualVenueConfig{Partition: defaultPaperPartition, RunTag: "run-1", MemberID: pairTestOKX, Provider: entryProviderOKX, AccountKey: "synthetic:okx-research",
			Instrument: "BTC-USDT-SWAP", Market: newPairTestMarket(), AdverseBandBps: 4}
		v, err := newPairVirtualVenue(c)
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		err = v.attach(pairVirtualAttachment{Store: e.store, Runtime: e.rt(), Config: e.bc.members[pairTestOKX], Clock: func() time.Time { return e.now }})
		e.mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "event 5000 hash") || v.isAttached() {
			t.Fatalf("attachment must refuse a tampered middle row: %v", err)
		}
		ident, layout := pairTestIdentity(t, e.cfg)
		ro, err := openStateDBReadOnly(filepath.Join(e.dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		fi := &storageFileInspection{Role: storageRolePrimary, ScopesOwned: layout.scopesForRole(storageRolePrimary), PartitionsOwned: partitionTextList(layout.partitionsForRole(storageRolePrimary))}
		owned := map[PortfolioScope]bool{}
		for _, sc := range fi.ScopesOwned {
			owned[sc] = true
		}
		rej, err := inspectPairVirtualRows(ro, fi, ident, owned, nil)
		if err != nil || len(rej) != 1 || !strings.Contains(rej[0], "is corrupt") {
			t.Fatalf("inspection must report a tampered middle row as corrupt: %v %v", rej, err)
		}
	})
	t.Run("allocation reads only the newest allocation", func(t *testing.T) {
		e, _ := newPairVirtualEnv(t, nil, 0, 0)
		e.step(pairTestStart().Add(-30 * time.Second))
		okx := e.vvenue(pairTestOKX)
		sid, key, err := e.db().pairVirtualKey(okx.id)
		if err != nil {
			t.Fatal(err)
		}
		prefix := "vp-run-1-" + okx.memberHash() + "-"
		tx, err := e.db().db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for seq := 1; seq <= 10000; seq++ {
			if seq == 5000 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO pair_virtual_allocations (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument, client_order_id, created_at_ms)
				VALUES ('run-1', ?, ?, 'paper', '', ?, ?, 'okx', 'synthetic:okx-research', 'BTC-USDT-SWAP', ?, 1)`, key, seq, pairTestID, sid, fmt.Sprintf("%s%d", prefix, seq)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		newest := prefix + "10000"
		if _, err := e.store.AppendPairVirtualEvents(e.rt(), okx.id, okx.binding, []pairVirtualEvent{{Kind: pairVirtualKindRefusal, SemanticKey: "refusal|" + newest, ClientOrderID: newest, Body: `{}`}}, nil, e.now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		got, err := okx.NewClientOrderID()
		if err != nil || got != prefix+"10001" {
			t.Fatalf("with 9,999 saved allocations, a hole at seq 5000 and a refused newest identifier with no action, allocation must read only the newest row and commit seq 10001: %q %v", got, err)
		}
		again, err := okx.NewClientOrderID()
		if err != nil || again != got {
			t.Fatalf("an allocation with no action and no venue record must be returned again after a crash: %q then %q %v", got, again, err)
		}
		if n := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations WHERE client_order_id = ?`, newest); n != 1 {
			t.Fatalf("the refused identifier must stay one spent allocation, got %d", n)
		}
		if _, err := e.store.LoadPairVirtualLedger(e.rt(), okx.id); err == nil || !strings.Contains(err.Error(), "has a hole") {
			t.Fatalf("a full load must refuse the allocation hole: %v", err)
		}
	})
	t.Run("ordinary load refuses allocation-only state", func(t *testing.T) {
		e, _ := newPairVirtualEnv(t, nil, 0, 0)
		e.step(pairTestStart().Add(-30 * time.Second))
		if _, err := e.vvenue(pairTestOKX).NewClientOrderID(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db().db.Exec(`DELETE FROM basket_contracts`); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`); n != 1 {
			t.Fatalf("want one allocation, got %d", n)
		}
		ordinary := *e.cfg
		ordinary.Pairs, ordinary.pairAdmission = nil, nil
		_, err := e.store.LoadBaskets(NewAppState(), &ordinary)
		if err == nil || !strings.Contains(err.Error(), "virtual venue records") {
			t.Fatalf("an ordinary load must refuse a file holding only virtual allocations, got %v", err)
		}
		if n := e.count(`SELECT COUNT(*) FROM pair_virtual_allocations`); n != 1 {
			t.Fatalf("the refused load changed the allocation records (%d)", n)
		}
	})
}

func pairTestIdentity(t *testing.T, cfg *Config) (storageIdentityMap, storageLayout) {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	return ident, layout
}

func TestBasketPairVirtualVenueLiquidityIsUsedOnce(t *testing.T) {
	e, market, a := newPairVirtualFirstLeg(t, 0, 0)
	s := a.SubmittedAtMs
	thin := [][2]string{{"60099.9", "2"}, {"60099.8", "2"}, {"60099.7", "1"}, {"60099.6", "1"}, {"60099.5", "1"}}
	book := e.bookObs(pairTestOKX, s+10, s+20, thin, e.quotes.okxAsks)
	copyLater := e.bookObs(pairTestOKX, s+10, s+400, thin, e.quotes.okxAsks)
	market.add(pairTestOKX, book, book, copyLater)
	okx := e.vvenue(pairTestOKX)
	hits := pairFailCommitOnce(t, "appendPairVirtualTx")
	e.stepAt(s + 500)
	storeCommitHook = nil
	if *hits != 1 || a.Status != basketActionUnresolved || e.virtualRows(pairVirtualKindMatch) != 0 || e.virtualRows(pairVirtualKindConsume) != 0 {
		t.Fatalf("a failed execution commit must expose nothing and consume nothing, got %s with %d matches", a.Status, e.virtualRows(pairVirtualKindMatch))
	}
	e.now = time.UnixMilli(s + 600).UTC()
	cp := *a
	var wg sync.WaitGroup
	outs := make([]basketVenueOutcome, 8)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := cp
			outs[i], _ = okx.Query(&c)
		}(i)
	}
	wg.Wait()
	for _, o := range outs {
		if o.Outcome != basketVenueFilled || fmt.Sprint(o.Fills) != fmt.Sprint(outs[0].Fills) {
			t.Fatalf("concurrent queries must share one execution, got %+v and %+v", o, outs[0])
		}
	}
	if e.virtualRows(pairVirtualKindMatch) != 1 || e.virtualRows(pairVirtualKindConsume) != 4 {
		t.Fatalf("copies and concurrent queries must consume each level once, got %d matches and %d consumptions", e.virtualRows(pairVirtualKindMatch), e.virtualRows(pairVirtualKindConsume))
	}
	var total string
	if err := e.db().db.QueryRow(`SELECT group_concat(consumed_native_qty, ',') FROM (SELECT consumed_native_qty FROM pair_virtual_events WHERE kind = 'consume' ORDER BY level_index)`).Scan(&total); err != nil || total != "2,2,1,1" {
		t.Fatalf("the full request must take each recorded level at most once, got %q %v", total, err)
	}
	var obsID string
	if err := e.db().db.QueryRow(`SELECT observation_id FROM pair_virtual_events WHERE kind = 'consume' AND level_index = 0`).Scan(&obsID); err != nil {
		t.Fatal(err)
	}
	before := e.virtualFootprint()
	over := []pairVirtualCapacity{{SourceContract: pairContractOKXPublic, ObservationID: obsID, BookSide: "sell", LevelIndex: 0, LevelPx: "60099.9", LevelQty: big.NewRat(2, 1), Consume: big.NewRat(1, 1)}}
	if _, err := e.store.AppendPairVirtualEvents(e.rt(), okx.id, okx.binding, []pairVirtualEvent{{Kind: pairVirtualKindConsume, SemanticKey: "consume|forged", ClientOrderID: a.ClientOrderID,
		ObservationID: obsID, SourceContract: pairContractOKXPublic, BookSide: "sell", LevelIndex: 0, ConsumedNative: "1", ConsumedBase: "0.01", Body: `{}`}}, over, e.now.UnixMilli()); err == nil {
		t.Fatalf("a consumption beyond the recorded level must refuse")
	}
	changed := []pairVirtualCapacity{{SourceContract: pairContractOKXPublic, ObservationID: obsID, BookSide: "sell", LevelIndex: 0, LevelPx: "60099.9", LevelQty: big.NewRat(9, 1), Consume: big.NewRat(1, 1)}}
	if _, err := e.store.AppendPairVirtualEvents(e.rt(), okx.id, okx.binding, nil, changed, e.now.UnixMilli()); err == nil {
		t.Fatalf("a level whose saved content differs under the same identity must refuse")
	}
	if after := e.virtualFootprint(); after != before {
		t.Fatalf("refused capacity checks changed the saved records")
	}
	e.stepAt(s + 700)
	if a.Status != basketActionCommitted || !pairNear(a.ConfirmedQty, 0.06) {
		t.Fatalf("the coordinator must book the saved execution once, got %s %.10g", a.Status, a.ConfirmedQty)
	}
}

func (e *pairTestEnv) sealedWindow(cutoff int64, completeLatest bool, tamper bool) pairObservation {
	e.t.Helper()
	m := e.member(pairTestHL)
	need := e.rt().Contract.Pair.BookNeed
	key := l2BookKeyFor(m.Instrument)
	b := &marketSnapshotBook{Key: key, Need: contractBookNeed(need.WindowMs, need.Capacity, need.Depth), Instance: "fixture-feed", CutoffMs: cutoff,
		CoverageFromMs: cutoff - 2*need.WindowMs, Obs: []feedBookObservation{}, Markers: []feedBookMarker{}}
	var recvs []int64
	for r := cutoff - 100; r >= cutoff-need.WindowMs; r -= 1000 {
		recvs = append(recvs, r)
	}
	sort.Slice(recvs, func(i, j int) bool { return recvs[i] < recvs[j] })
	q := e.quotes
	for i, r := range recvs {
		bl, al := pairFixtureBookLevels(q.hlBids, need.Depth), pairFixtureBookLevels(q.hlAsks, need.Depth)
		bd, ad := len(bl), len(al)
		latest := i == len(recvs)-1
		if !(latest && completeLatest) {
			al, ad = al[:2], 10
		}
		id := bookIdentity(key, r-50, bl, al)
		if tamper && latest {
			id = bookIdentity(key, r-51, bl, al)
		}
		b.Obs = append(b.Obs, feedBookObservation{Seq: uint64(i + 1), EventMs: r - 50, RecvMs: r, Session: 1, Identity: id, BidDepth: bd, AskDepth: ad, Bids: bl, Asks: al})
	}
	snap := &marketSnapshot{EvaluationID: fmt.Sprintf("seal-%d", cutoff), ConfigGeneration: 1, Deadline: time.UnixMilli(cutoff).UTC(), books: map[feedObservationKey]*marketSnapshotBook{key: b}}
	payload := pairFixtureCanonical(snap.l2BookPayload(key, b.Need))
	obs, err := pairObservationFromPayload(m, e.rt().Partition.String(), need, payload, 0, 1)
	if err != nil {
		e.t.Fatalf("sealed window at %d: %v", cutoff, err)
	}
	return obs
}

func TestBasketPairVirtualVenueAbsenceNeedsContinuousRecordedHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		windows func(e *pairTestEnv, h int64) []pairObservation
		release bool
	}{
		{name: "continuous windows", release: true, windows: func(e *pairTestEnv, h int64) []pairObservation {
			c := h + 30150
			return []pairObservation{e.sealedWindow(c, true, false), e.sealedWindow(c+60000, true, false), e.sealedWindow(c+120000, true, false)}
		}},
		{name: "missing middle window", windows: func(e *pairTestEnv, h int64) []pairObservation {
			c := h + 30150
			return []pairObservation{e.sealedWindow(c, true, false), e.sealedWindow(c+150000, true, false)}
		}},
		{name: "record identity conflict", windows: func(e *pairTestEnv, h int64) []pairObservation {
			c := h + 30150
			return []pairObservation{e.sealedWindow(c, true, false), e.sealedWindow(c+60000, true, true), e.sealedWindow(c+120000, true, false)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, market := newPairVirtualEnv(t, func(root map[string]interface{}) { pairTestMemberEntry(root, 0)["source"] = pairSourceSealedFeed }, 0, 0)
			e.virtualFirstLegFilled(market)
			e.slotStep(1)
			h := e.actionsByPurpose(pairPurposeEntry)[1]
			if h.StrategyID != pairTestHL {
				t.Fatalf("want the hyperliquid hedge, got %+v", h)
			}
			start := h.SubmittedAtMs
			market.add(pairTestHL, tc.windows(e, start)...)
			market.coverage = func(key pairMemberKey, after, through int64) pairCoverage {
				spec, _ := pairSourceSpecFor(entryProviderHyperliquid, pairSourceSealedFeed)
				top := after
				for _, o := range market.events[pairTestHL] {
					if o.FirstReceiptMs <= through && o.SourceCutoffMs > top && o.SourceCutoffMs <= through {
						top = o.SourceCutoffMs
					}
				}
				return pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics, FromMs: after, ThroughMs: top,
					PrefixComplete: true, Status: pairCoverageComplete, Gaps: []pairCoverageGap{}}
			}
			limit := h.AcceptDeadlineMs + entryRecoveryGrace.Milliseconds()
			e.stepAt(limit)
			h = e.rt().Actions[h.ActionID]
			if h.Status != basketActionUnresolved || e.virtualRows(pairVirtualKindAbsence) != 0 {
				t.Fatalf("absence must not release at exactly the deadline plus grace, got %s", h.Status)
			}
			e.stepAt(limit + 200)
			released := e.virtualRows(pairVirtualKindAbsence) == 1
			if released != tc.release || e.virtualRows(pairVirtualKindMatch) != 1 {
				t.Fatalf("absence released %v, want %v (matches %d)", released, tc.release, e.virtualRows(pairVirtualKindMatch))
			}
			got := e.rt().Actions[h.ActionID]
			if tc.release && (got == nil || got.Status != basketActionNoFill || e.lifecycle() != pairLifecycleRecovery) {
				t.Fatalf("a proven absent hedge must end with no fill and open recovery, got %+v %s", got, e.lifecycle())
			}
			if !tc.release && (got == nil || got.Status != basketActionUnresolved) {
				t.Fatalf("without continuous history the hedge must stay unresolved, got %+v", got)
			}
		})
	}
}

func TestBasketPairVirtualVenuePartialAndZeroIOCAreTerminal(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		e, market, a := newPairVirtualFirstLeg(t, 0, 0)
		s := a.SubmittedAtMs
		thin := [][2]string{{"60099.9", "2"}, {"60099.8", "2"}, {"59000", "9"}, {"58999", "9"}, {"58998", "9"}}
		market.add(pairTestOKX, e.bookObs(pairTestOKX, s+10, s+10, thin, e.quotes.okxAsks), e.defaultBook(pairTestOKX, s+20, s+20))
		e.stepAt(s + 30)
		out := e.outcomeOf(a)
		if out.Outcome != basketVenuePartial || !out.Terminal || !pairNear(a.ConfirmedQty, 0.04) || !a.Terminal {
			t.Fatalf("a short first book must give a terminal partial, got %+v confirmed %.10g", out, a.ConfirmedQty)
		}
		e.assertBooks("partial first leg", 0, -0.04)
		if _, ok := e.vvenue(pairTestOKX).NextVisibleMs(); ok {
			t.Fatalf("a published visibility must leave no past wake")
		}
	})
	t.Run("zero", func(t *testing.T) {
		e, market, a := newPairVirtualFirstLeg(t, 0, 0)
		s := a.SubmittedAtMs
		low := [][2]string{{"59700", "9"}, {"59699", "9"}, {"59698", "9"}, {"59697", "9"}, {"59696", "9"}}
		market.add(pairTestOKX, e.bookObs(pairTestOKX, s+10, s+10, low, e.quotes.okxAsks), e.defaultBook(pairTestOKX, s+20, s+20))
		e.stepAt(s + 30)
		if a.Status != basketActionNoFill || !a.Terminal || a.ConfirmedQty != 0 || e.virtualRows(pairVirtualKindConsume) != 0 {
			t.Fatalf("zero admissible depth after the adverse rate must end the IOC with no fill, got %s %.10g", a.Status, a.ConfirmedQty)
		}
		e.assertBooks("no fill", 0, 0)
		if e.lifecycle() != pairLifecycleFlat {
			t.Fatalf("a rejected first leg leaves the pair flat, got %s", e.lifecycle())
		}
	})
}
