package main

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

type pairVirtualMarket interface {
	Events(ctx context.Context, key pairMemberKey, afterMs, throughMs int64) (pairVirtualMarketWindow, error)
}

type pairVirtualMarketWindow struct {
	Events   []pairObservation
	Coverage pairCoverage
}

type pairVirtualBook struct {
	SourceContract  string            `json:"source_contract"`
	SourceID        string            `json:"source_id"`
	PayloadHash     string            `json:"payload_hash"`
	EventMs         int64             `json:"event_ms"`
	ReceiptMs       int64             `json:"receipt_ms"`
	AvailableMs     int64             `json:"available_ms"`
	Bids            []pairNativeLevel `json:"bids"`
	Asks            []pairNativeLevel `json:"asks"`
	BidDepth        int               `json:"bid_depth"`
	AskDepth        int               `json:"ask_depth"`
	IdentityChecked bool              `json:"identity_checked"`
	order           int
	bids, asks      []pairLevelRat
}

type pairVirtualContinuity struct {
	Continuous bool
	FromMs     int64
	ThroughMs  int64
	Reason     string
}

func (b pairVirtualBook) levels(side string) ([]pairLevelRat, int) {
	if side == "buy" {
		return b.asks, b.AskDepth
	}
	return b.bids, b.BidDepth
}

func (b pairVirtualBook) complete(side string, need pairBookNeed) bool {
	levels, declared := b.levels(side)
	want := declared
	if want > need.Depth {
		want = need.Depth
	}
	if want < 1 || len(levels) != want || len(b.bids) == 0 || len(b.asks) == 0 {
		return false
	}
	for i := 1; i < len(b.bids); i++ {
		if b.bids[i].px.Cmp(b.bids[i-1].px) >= 0 {
			return false
		}
	}
	for i := 1; i < len(b.asks); i++ {
		if b.asks[i].px.Cmp(b.asks[i-1].px) <= 0 {
			return false
		}
	}
	return b.bids[0].px.Cmp(b.asks[0].px) < 0
}

func (b pairVirtualBook) sameContent(o pairVirtualBook) bool {
	if b.SourceContract != o.SourceContract || b.SourceID != o.SourceID || b.EventMs != o.EventMs || b.BidDepth != o.BidDepth || b.AskDepth != o.AskDepth {
		return false
	}
	eq := func(x, y []pairNativeLevel) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return eq(b.Bids, o.Bids) && eq(b.Asks, o.Asks)
}

func pairVirtualLevelRats(levels []pairNativeLevel) ([]pairLevelRat, error) {
	out := make([]pairLevelRat, 0, len(levels))
	for _, l := range levels {
		px, ok := new(big.Rat).SetString(l.Px)
		if !ok || px.Sign() <= 0 || !pairDecimalCanonical(l.Px) {
			return nil, fmt.Errorf("level price %q is not a positive canonical decimal", l.Px)
		}
		q, ok := new(big.Rat).SetString(l.Qty)
		if !ok || q.Sign() <= 0 || !pairDecimalCanonical(l.Qty) {
			return nil, fmt.Errorf("level quantity %q is not a positive canonical decimal", l.Qty)
		}
		out = append(out, pairLevelRat{px: px, qty: q, level: l})
	}
	return out, nil
}

func pairVirtualNativeLevels(levels []pairLevelRat) []pairNativeLevel {
	out := make([]pairNativeLevel, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.level)
	}
	return out
}

func pairVirtualMax(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func pairVirtualBooks(m pairContractMember, key pairEvidenceKey, need pairBookNeed, side string, w pairVirtualMarketWindow, afterMs, throughMs int64) ([]pairVirtualBook, pairVirtualContinuity, error) {
	spec, ok := pairSourceSpecFor(m.Provider, m.Source)
	if !ok {
		return nil, pairVirtualContinuity{}, fmt.Errorf("member %s source %q has no source contract", m.MemberID, m.Source)
	}
	cov := w.Coverage
	if cov.SourceContract != spec.Contract || cov.TimeSemantics != spec.TimeSemantics || cov.IdentitySemantics != spec.IdentitySemantics {
		return nil, pairVirtualContinuity{}, fmt.Errorf("the market window coverage names %s (%s, %s), the member source is %s (%s, %s)", cov.SourceContract, cov.TimeSemantics, cov.IdentitySemantics, spec.Contract, spec.TimeSemantics, spec.IdentitySemantics)
	}
	if cov.FromMs > cov.ThroughMs || cov.ThroughMs > throughMs {
		return nil, pairVirtualContinuity{}, fmt.Errorf("the market window coverage [%d, %d] is inverted or extends past the request bound %d", cov.FromMs, cov.ThroughMs, throughMs)
	}
	if cov.Status != pairCoverageComplete && cov.Status != pairCoverageIncomplete {
		return nil, pairVirtualContinuity{}, fmt.Errorf("the market window coverage status %q is not known", cov.Status)
	}
	for _, g := range cov.Gaps {
		if g.FromMs > g.ThroughMs || g.FromMs < cov.FromMs || g.ThroughMs > cov.ThroughMs {
			return nil, pairVirtualContinuity{}, fmt.Errorf("the market window coverage gap [%d, %d] lies outside its coverage [%d, %d]", g.FromMs, g.ThroughMs, cov.FromMs, cov.ThroughMs)
		}
	}
	var books []pairVirtualBook
	seen := map[string]int{}
	var lastOrder int64
	var spans []pairVirtualSpan
	for i, o := range w.Events {
		if _, err := pairCheckObservationSourceKey(o, key); err != nil {
			return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d: %w", i, err)
		}
		_, d, err := pairCheckObservationEvent(o, m, key, need, side)
		if err != nil {
			return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d is malformed: %w", i, err)
		}
		venue := *o.VenueTimeMs
		if venue <= afterMs || venue > throughMs {
			return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d venue time %d lies outside the requested window (%d, %d]", i, venue, afterMs, throughMs)
		}
		if o.FirstReceiptMs > throughMs {
			return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d was first received at %d, after the request bound %d; a future record never reaches execution", i, o.FirstReceiptMs, throughMs)
		}
		order := o.FirstReceiptMs
		if !spec.Point {
			order = o.SourceCutoffMs
		}
		if i > 0 && order < lastOrder {
			return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d breaks the recorded source order (%d after %d)", i, order, lastOrder)
		}
		lastOrder = order
		var cands []pairVirtualBook
		if spec.Point {
			bids, err := pairVirtualLevelRats(d.Bids)
			if err != nil {
				return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d: %w", i, err)
			}
			asks, err := pairVirtualLevelRats(d.Asks)
			if err != nil {
				return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d: %w", i, err)
			}
			cands = append(cands, pairVirtualBook{SourceContract: spec.Contract, SourceID: o.PayloadHash, PayloadHash: o.PayloadHash, EventMs: d.VenueTimeMs, ReceiptMs: d.FirstReceiptMs,
				Bids: d.Bids, Asks: d.Asks, BidDepth: len(d.Bids), AskDepth: len(d.Asks), IdentityChecked: true, bids: bids, asks: asks})
		} else {
			if o.Coverage.FromMs <= o.Coverage.ThroughMs && o.Coverage.Status == pairCoverageComplete && len(o.Coverage.Gaps) == 0 && o.Coverage.PrefixComplete {
				spans = append(spans, pairVirtualSpan{o.Coverage.FromMs, o.Coverage.ThroughMs})
			}
			for _, r := range d.Records {
				checked := false
				if len(r.Bids) == r.BidDepth && len(r.Asks) == r.AskDepth {
					if got := bookIdentity(l2BookKeyFor(m.Instrument), r.EventMs, pairVirtualFeedLevels(r.Bids), pairVirtualFeedLevels(r.Asks)); got != r.Identity {
						return nil, pairVirtualContinuity{}, fmt.Errorf("market event %d record %d identity %s does not hash its levels", i, r.Seq, r.Identity)
					}
					checked = true
				}
				cands = append(cands, pairVirtualBook{SourceContract: spec.Contract, SourceID: r.Identity, PayloadHash: o.PayloadHash, EventMs: r.EventMs, ReceiptMs: r.ReceiptMs,
					Bids: pairVirtualNativeLevels(r.Bids), Asks: pairVirtualNativeLevels(r.Asks), BidDepth: r.BidDepth, AskDepth: r.AskDepth, IdentityChecked: checked, bids: r.Bids, asks: r.Asks})
			}
		}
		for _, b := range cands {
			b.AvailableMs = pairVirtualMax(pairVirtualMax(b.EventMs, b.ReceiptMs), o.FirstReceiptMs)
			if j, dup := seen[b.SourceID]; dup {
				if !books[j].sameContent(b) {
					return nil, pairVirtualContinuity{}, fmt.Errorf("source identity %s carries conflicting content in two market records", b.SourceID)
				}
				if b.AvailableMs < books[j].AvailableMs {
					books[j].AvailableMs, books[j].ReceiptMs = b.AvailableMs, b.ReceiptMs
				}
				continue
			}
			if b.ReceiptMs > throughMs {
				continue
			}
			b.order = len(books)
			seen[b.SourceID] = len(books)
			books = append(books, b)
		}
	}
	cont := pairVirtualContinuity{Reason: "point snapshot sources prove no continuity between polls"}
	if !spec.Point {
		cont = pairVirtualWindowContinuity(cov, spans)
	}
	return books, cont, nil
}

func pairVirtualFeedLevels(levels []pairLevelRat) []feedBookLevel {
	out := make([]feedBookLevel, 0, len(levels))
	for _, l := range levels {
		out = append(out, feedBookLevel{Px: l.level.Px, Sz: l.level.Qty, N: l.orders})
	}
	return out
}

func pairCheckObservationSourceKey(o pairObservation, key pairEvidenceKey) (string, error) {
	if o.Key != key {
		return "", fmt.Errorf("observation key %s is not the member key %s", o.Key, key)
	}
	return o.PayloadHash, nil
}

type pairVirtualSpan struct{ from, through int64 }

func pairVirtualWindowContinuity(cov pairCoverage, spans []pairVirtualSpan) pairVirtualContinuity {
	out := pairVirtualContinuity{FromMs: cov.FromMs, ThroughMs: cov.ThroughMs}
	switch {
	case cov.Status != pairCoverageComplete:
		out.Reason = "the market window coverage is " + cov.Status
		return out
	case !cov.PrefixComplete:
		out.Reason = "the market window prefix is incomplete"
		return out
	case len(cov.Gaps) > 0:
		out.Reason = fmt.Sprintf("the market window coverage has %d gaps", len(cov.Gaps))
		return out
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].from != spans[j].from {
			return spans[i].from < spans[j].from
		}
		return spans[i].through < spans[j].through
	})
	var merged []pairVirtualSpan
	for _, s := range spans {
		if n := len(merged); n > 0 && s.from <= merged[n-1].through {
			if s.through > merged[n-1].through {
				merged[n-1].through = s.through
			}
			continue
		}
		merged = append(merged, s)
	}
	for _, s := range merged {
		if s.from <= cov.FromMs && s.through >= cov.ThroughMs {
			out.Continuous = true
			return out
		}
	}
	var parts []string
	for _, s := range merged {
		parts = append(parts, fmt.Sprintf("[%d, %d]", s.from, s.through))
	}
	out.Reason = fmt.Sprintf("the claimed coverage [%d, %d] is not inside one complete span of the recorded windows (%s)", cov.FromMs, cov.ThroughMs, strings.Join(parts, ", "))
	return out
}
