package main

import (
	"errors"
	"math"
	"testing"
)

type hlShareRowBook struct {
	name  string
	qty   float64
	armed bool
}

func TestHLOwnStopShare(t *testing.T) {
	long := "long"
	short := "short"
	both := func(a, b float64) []hlShareRowBook {
		return []hlShareRowBook{{"A", a, true}, {"B", b, true}}
	}
	cases := []struct {
		name    string
		side    string
		books   []hlShareRowBook
		opp     float64
		signed  float64
		known   bool
		want    map[string]float64
		sideSum *float64
	}{
		{name: "drift both resting long", side: long, books: both(10, 5), signed: 12, known: true, want: map[string]float64{"A": 8, "B": 4}, sideSum: num64(12)},
		{name: "opposite-side net with no drift", side: long, books: []hlShareRowBook{{"A", 10, true}}, opp: 4, signed: 6, known: true, want: map[string]float64{"A": 6}},
		{name: "short peer on a long net gets nothing", side: short, books: []hlShareRowBook{{"B", 4, true}}, opp: 10, signed: 6, known: true, want: map[string]float64{"B": 0}},
		{name: "two longs and a short net stay at the book", side: long, books: []hlShareRowBook{{"A", 5, true}, {"A2", 5, true}}, opp: 4, signed: 6, known: true, want: map[string]float64{"A": 5, "A2": 5}},
		{name: "long book on a short net is zero", side: long, books: []hlShareRowBook{{"A", 4, true}}, opp: 10, signed: -6, known: true, want: map[string]float64{"A": 0}},
		{name: "short drift both resting", side: short, books: []hlShareRowBook{{"A", 9, true}, {"B", 3, true}}, signed: -8, known: true, want: map[string]float64{"A": 6, "B": 2}, sideSum: num64(8)},
		{name: "no drift keeps each book", side: long, books: both(5, 5), signed: 12, known: true, want: map[string]float64{"A": 5, "B": 5}, sideSum: num64(10)},
		{name: "sole owner caps at the chain", side: long, books: []hlShareRowBook{{"A", 10, true}}, signed: 7, known: true, want: map[string]float64{"A": 7}, sideSum: num64(7)},
		{name: "unarmed excess is cut before a resting stop", side: long, books: []hlShareRowBook{{"A", 3, false}, {"B", 5, true}}, signed: 5, known: true, want: map[string]float64{"A": 0, "B": 5}, sideSum: num64(5)},
		{name: "deep drift both resting", side: long, books: both(10, 5), signed: 6, known: true, want: map[string]float64{"A": 4, "B": 2}, sideSum: num64(6)},
		{name: "resting book keeps its size and the new book takes the rest", side: long, books: []hlShareRowBook{{"A", 10, true}, {"B", 5, false}}, signed: 12, known: true, want: map[string]float64{"A": 10, "B": 2}, sideSum: num64(12)},
		{name: "flat chain on a known view", side: long, books: []hlShareRowBook{{"A", 10, true}}, signed: 0, known: true, want: map[string]float64{"A": 0}, sideSum: num64(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hlShareRowQtys(tc.side, tc.books, tc.opp, tc.signed, tc.known)
			var sum float64
			for _, bk := range tc.books {
				q := got[bk.name]
				sum += q
				own := 0.0
				if tc.known {
					own = math.Max(hlSideSign(tc.side)*tc.signed, 0)
				}
				if tc.known && q > math.Min(bk.qty, own)+1e-9 {
					t.Fatalf("%s Q=%g exceeds min(book %g, own %g)", bk.name, q, bk.qty, own)
				}
				if math.Abs(q-tc.want[bk.name]) > 1e-9 {
					t.Fatalf("%s Q=%g, want %g", bk.name, q, tc.want[bk.name])
				}
			}
			if tc.sideSum != nil && math.Abs(sum-*tc.sideSum) > 1e-9 {
				t.Fatalf("side sum %g, want %g", sum, *tc.sideSum)
			}
		})
	}

	t.Run("unknown view gives the book", func(t *testing.T) {
		res := hlOwnStopShare(hlShareInput{Side: "long", Self: hlShareBook{Qty: 10, Armed: true}})
		if res.Known || math.Abs(res.Qty-10) > 1e-9 {
			t.Fatalf("got %+v, want the book and known false", res)
		}
	})
	t.Run("a sideless size is an unknown view", func(t *testing.T) {
		view := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 6}}
		res := hlOwnStopShareOnView("ETH", "long", hlShareBook{Qty: 10, Armed: true}, nil, 0, view)
		if res.Known || math.Abs(res.Qty-10) > 1e-9 {
			t.Fatalf("got %+v, want the book and known false", res)
		}
	})
	t.Run("a mixed-case coin reads the chain", func(t *testing.T) {
		view := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"eth": 7}, NetSide: map[string]string{"eth": "long"}}
		res := hlOwnStopShareOnView("ETH", "long", hlShareBook{Qty: 10, Armed: true}, nil, 0, view)
		if !res.Known || math.Abs(res.Qty-7) > 1e-9 {
			t.Fatalf("got %+v, want Q 7", res)
		}
	})
}

func num64(v float64) *float64 { return &v }

func hlAbsShare(abs map[string]float64, side string) *hlCycleShare {
	if abs == nil {
		return nil
	}
	net := map[string]string{}
	for k, v := range abs {
		if v > 1e-9 && side != "" {
			net[k] = side
		}
	}
	return newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: abs, NetSide: net}, hlCoinSubmitSnapshot(), nil, nil, nil, nil)
}

func hlShareRowQtys(side string, books []hlShareRowBook, opp, signed float64, known bool) map[string]float64 {
	out := map[string]float64{}
	for i, self := range books {
		var same []hlShareBook
		for j, peer := range books {
			if i == j {
				continue
			}
			same = append(same, hlShareBook{Qty: peer.qty, Armed: peer.armed})
		}
		res := hlOwnStopShare(hlShareInput{
			Side: side, Self: hlShareBook{Qty: self.qty, Armed: self.armed},
			Same: same, Opp: opp, Signed: signed, Known: known,
		})
		out[self.name] = res.Qty
	}
	return out
}

func TestHLCycleShareFreshnessAndLatch(t *testing.T) {
	resetHLShareAlerts()
	t.Cleanup(resetHLShareAlerts)

	longA := StrategyConfig{ID: "A", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	shortB := StrategyConfig{ID: "B", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	states := map[string]*StrategyState{
		"A": {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, StopLossOID: 1, StopLossTriggerPx: 90}}},
		"B": {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "short", Quantity: 4, StopLossOID: 2, StopLossTriggerPx: 110}}},
	}
	live := []StrategyConfig{longA, shortB}
	start := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 10}, NetSide: map[string]string{"ETH": "long"}}
	snap := hlCoinSubmitSnapshot()
	reads := 0
	refetch := func() (hlOnChainCoinView, error) {
		reads++
		return hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 6}, NetSide: map[string]string{"ETH": "long"}}, nil
	}
	share := newHLCycleShare(start, snap, refetch, states, live, nil)
	peers, opp := share.peers("ETH", "A", "long")
	q := share.StopQty(longA, "ETH", "long", 10, true, peers, opp)
	if reads != 0 || !q.Fresh || !q.Known || math.Abs(q.Qty-10) > 1e-9 {
		t.Fatalf("no submission: qty=%g fresh=%t known=%t reads=%d, want 10 with no second read", q.Qty, q.Fresh, q.Known, reads)
	}

	hlNoteCoinSubmission("ETH")
	q = share.StopQty(longA, "ETH", "long", 10, true, peers, opp)
	if reads != 1 || math.Abs(q.Qty-6) > 1e-9 || !q.Capped {
		t.Fatalf("after B's submission: qty=%g capped=%t reads=%d, want 6 and one read", q.Qty, q.Capped, reads)
	}

	failShare := newHLCycleShare(start, hlCoinSubmitSnapshot(), func() (hlOnChainCoinView, error) {
		return hlOnChainCoinView{}, errors.New("timeout")
	}, states, live, nil)
	hlNoteCoinSubmission("ETH")
	failed := failShare.StopQty(longA, "ETH", "long", 10, true, peers, opp)
	if failed.Fresh || failed.Known || math.Abs(failed.Qty-10) > 1e-9 {
		t.Fatalf("failed refetch: %+v, want the book and not fresh", failed)
	}

	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	driftView := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 12}, NetSide: map[string]string{"ETH": "long"}}
	alertShare := newHLCycleShare(driftView, hlCoinSubmitSnapshot(), nil, states, live, mn)
	aPeers := []hlShareBook{{Qty: 5, Armed: true}}
	first := alertShare.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	second := alertShare.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	if math.Abs(first.Qty-8) > 1e-9 || len(mock.dms) != 1 {
		t.Fatalf("first unbacked Q=%g alerts=%d, want Q 8 and one alert", first.Qty, len(mock.dms))
	}
	if math.Abs(second.Qty-first.Qty) > 1e-9 || len(mock.dms) != 1 {
		t.Fatalf("repeat sent %d alerts, want 1", len(mock.dms))
	}
	changed := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 6}, NetSide: map[string]string{"ETH": "long"}}, hlCoinSubmitSnapshot(), nil, states, live, mn)
	changed.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	if len(mock.dms) != 2 {
		t.Fatalf("changed Q sent %d alerts, want 2", len(mock.dms))
	}
	cleared := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 15}, NetSide: map[string]string{"ETH": "long"}}, hlCoinSubmitSnapshot(), nil, states, live, mn)
	cleared.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	again := newHLCycleShare(driftView, hlCoinSubmitSnapshot(), nil, states, live, mn)
	again.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	if len(mock.dms) != 3 {
		t.Fatalf("cleared latch then the same state sent %d alerts, want 3", len(mock.dms))
	}

	resetHLShareAlerts()
	unknownView := hlOnChainCoinView{Known: false}
	for i := 0; i < 3; i++ {
		s := newHLCycleShare(unknownView, hlCoinSubmitSnapshot(), nil, states, live, mn)
		s.StopQty(longA, "ETH", "long", 10, true, aPeers, 0)
	}
	if len(mock.dms) != 4 {
		t.Fatalf("three unknown cycles sent %d alerts after the latch cases, want 4", len(mock.dms))
	}
}
