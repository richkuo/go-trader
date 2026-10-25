package main

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"time"
)

type pairContractBinder interface {
	bindContract(c basketContract) error
}

type pairReplaySealedStream struct {
	member pairContractMember
	key    feedObservationKey
	doc    pairReplayBookDoc
	obs    []feedBookObservation
	marks  []feedBookMarker
}

type pairReplayQuoteSource struct {
	mu        sync.Mutex
	runID     string
	sources   map[string]string
	docs      map[string]pairReplayQuoteDoc
	bookDocs  map[string]pairReplayBookDoc
	partition string
	bound     string
	bindErr   error
	members   map[string]pairContractMember
	need      pairBookNeed
	public    map[string][]pairObservation
	sealed    map[string]*pairReplaySealedStream
}

func newPairReplayQuoteSource(runID string, sources map[string]string, in *pairReplayInputs) *pairReplayQuoteSource {
	return &pairReplayQuoteSource{runID: runID, sources: sources, docs: in.quotes, bookDocs: in.books}
}

func (s *pairReplayQuoteSource) Sources() map[string]string {
	out := make(map[string]string, len(s.sources))
	for k, v := range s.sources {
		out[k] = v
	}
	return out
}

func (s *pairReplayQuoteSource) Recorded() bool { return true }

func (s *pairReplayQuoteSource) bindContract(c basketContract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := c.Fingerprint()
	if s.bound != "" {
		if s.bound != fp {
			return fmt.Errorf("the recorded quote source is bound to contract %s, not %s", shortFingerprint(s.bound), shortFingerprint(fp))
		}
		return s.bindErr
	}
	s.bound = fp
	s.bindErr = s.bindLocked(c)
	return s.bindErr
}

func (s *pairReplayQuoteSource) bindLocked(c basketContract) error {
	s.partition = c.Partition
	s.need = c.Pair.BookNeed
	s.members = map[string]pairContractMember{}
	s.public = map[string][]pairObservation{}
	s.sealed = map[string]*pairReplaySealedStream{}
	var errs []string
	used := map[string]bool{}
	for _, m := range c.pairMembersSorted() {
		s.members[m.MemberID] = m
		switch m.Source {
		case pairSourcePublicSnapshot:
			doc, ok := s.docs[m.MemberID]
			if !ok {
				errs = append(errs, fmt.Sprintf("member %s reads %s but the input manifest has no quotes object for it", m.MemberID, m.Source))
				continue
			}
			used["quotes|"+m.MemberID] = true
			out := make([]pairObservation, 0, len(doc.Records))
			for i, r := range doc.Records {
				o, err := pairObservationFromPayload(m, c.Partition, s.need, r.Payload, r.ReceiptMs, 0)
				if err != nil {
					errs = append(errs, fmt.Sprintf("member %s recorded quote %d: %v", m.MemberID, i, err))
					break
				}
				if o.FirstReceiptMs != r.ReceiptMs {
					errs = append(errs, fmt.Sprintf("member %s recorded quote %d derives receipt %d, the record keeps %d", m.MemberID, i, o.FirstReceiptMs, r.ReceiptMs))
					break
				}
				out = append(out, o)
			}
			s.public[m.MemberID] = out
		case pairSourceSealedFeed:
			doc, ok := s.bookDocs[m.MemberID]
			if !ok {
				errs = append(errs, fmt.Sprintf("member %s reads %s but the input manifest has no books object for it", m.MemberID, m.Source))
				continue
			}
			used["books|"+m.MemberID] = true
			st := &pairReplaySealedStream{member: m, key: l2BookKeyFor(m.Instrument), doc: doc}
			for i, r := range doc.Records {
				st.obs = append(st.obs, feedBookObservation{Seq: uint64(i + 1), EventMs: r.EventMs, RecvMs: r.RecvMs, Session: 1,
					Identity: bookIdentity(st.key, r.EventMs, r.Bids, r.Asks), BidDepth: len(r.Bids), AskDepth: len(r.Asks), Bids: r.Bids, Asks: r.Asks})
			}
			for i, mk := range doc.Markers {
				st.marks = append(st.marks, feedBookMarker{Seq: uint64(i + 1), AtMs: mk.AtMs, Reason: mk.Reason, Session: 1, Detail: mk.Detail})
			}
			sort.SliceStable(st.marks, func(i, j int) bool { return st.marks[i].AtMs < st.marks[j].AtMs })
			s.sealed[m.MemberID] = st
		default:
			errs = append(errs, fmt.Sprintf("member %s source %q has no offline recorded source in the version-2 replay (public_snapshot and sealed_feed are supported)", m.MemberID, m.Source))
		}
	}
	for id := range s.docs {
		if !used["quotes|"+id] {
			errs = append(errs, fmt.Sprintf("the quotes object for %s names no public_snapshot member", id))
		}
	}
	for id := range s.bookDocs {
		if !used["books|"+id] {
			errs = append(errs, fmt.Sprintf("the books object for %s names no sealed_feed member", id))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("recorded quote input refused: %v", errs)
	}
	return nil
}

func (s *pairReplayQuoteSource) latestPublic(id string, atMs int64) (pairObservation, bool) {
	list := s.public[id]
	i := sort.Search(len(list), func(i int) bool { return list[i].FirstReceiptMs > atMs })
	if i == 0 {
		return pairObservation{}, false
	}
	return pairCloneObservation(list[i-1]), true
}

func (s *pairReplayQuoteSource) read(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	if err := ctx.Err(); err != nil {
		return pairObservation{}, err
	}
	s.mu.Lock()
	bound := s.bound != "" && s.bindErr == nil
	src := s.sources[r.Member.MemberID]
	s.mu.Unlock()
	if !bound {
		return pairObservation{}, fmt.Errorf("the recorded quote source is not bound to the admitted contract")
	}
	if src != r.Member.Source {
		return pairObservation{}, fmt.Errorf("member %s is recorded as %q, not %q", r.Member.MemberID, src, r.Member.Source)
	}
	if want := pairSourceContractFor(r.Member.Provider, r.Member.Source); want == "" || r.SourceContract != want {
		return pairObservation{}, fmt.Errorf("member %s source contract %q is not %q", r.Member.MemberID, r.SourceContract, want)
	}
	if r.Member.Source == pairSourceSealedFeed {
		return pairSealedReader{}.readQuote(ctx, r)
	}
	o, ok := s.latestPublic(r.Member.MemberID, r.StartMs)
	if !ok {
		return pairObservation{}, fmt.Errorf("member %s has no recorded quote received at or before %d; the recorded source never substitutes current data", r.Member.MemberID, r.StartMs)
	}
	return o, nil
}

func (s *pairReplayQuoteSource) Collect(ctx context.Context, req pairCollectRequest) ([]pairObservation, error) {
	if len(req.Members) == 0 {
		return nil, fmt.Errorf("the collection request names no member")
	}
	out := make([]pairObservation, 0, len(req.Members))
	for _, m := range req.Members {
		o, err := s.read(ctx, pairMemberRead{Partition: req.Partition, Member: m, BookNeed: req.BookNeed, SourceContract: req.SourceContracts[m.MemberID],
			StartMs: req.StartMs, Snapshot: req.Snapshot, SnapshotID: req.SnapshotID, Generation: req.Generation})
		if err != nil {
			return nil, fmt.Errorf("member %s %s read: %w", m.MemberID, m.Source, err)
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *pairReplayQuoteSource) Reference(ctx context.Context, req pairReferenceRequest) (pairObservation, error) {
	if req.Side != "buy" && req.Side != "sell" {
		return pairObservation{}, fmt.Errorf("reference side %q is neither buy nor sell", req.Side)
	}
	return s.read(ctx, pairMemberRead{Partition: req.Partition, Member: req.Member, BookNeed: req.BookNeed, SourceContract: req.SourceContract,
		StartMs: req.StartMs, Snapshot: req.Snapshot, SnapshotID: req.SnapshotID, Generation: req.Generation, Side: req.Side})
}

func (s *pairReplayQuoteSource) seal(rt *basketRuntime, at time.Time) *marketSnapshot {
	atMs := at.UnixMilli()
	needs := pairFeedBookNeeds(rt)
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: fmt.Sprintf("replay/%s/seal/%d", s.runID, atMs), ConfigGeneration: 1,
		SealedAt: at, Deadline: at, books: map[feedObservationKey]*marketSnapshotBook{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range sortedBookNeedKeys(needs) {
		for _, st := range s.sealed {
			if st.key != k {
				continue
			}
			snap.books[k] = st.freeze(needs[k], atMs)
		}
	}
	return snap
}

func (st *pairReplaySealedStream) freeze(need feedBookNeed, atMs int64) *marketSnapshotBook {
	n := sort.Search(len(st.obs), func(i int) bool { return st.obs[i].RecvMs > atMs })
	m := sort.Search(len(st.marks), func(i int) bool { return st.marks[i].AtMs > atMs })
	from := st.doc.CoverageFromMs
	state := &feedBookState{Key: st.key, Need: need, Obs: st.obs[:n], Markers: st.marks[:m], Covered: atMs >= from && atMs <= st.doc.CoverageThroughMs,
		Session: 1, RetainedFromMs: from, DepthFromMs: from, StartedMs: from}
	if n > 0 {
		state.HighRecvMs, state.HighEventMs, state.LastAcceptMs = st.obs[n-1].RecvMs, st.obs[n-1].EventMs, st.obs[n-1].RecvMs
	}
	return freezeBook(state, need, st.doc.Instance, atMs, atMs)
}

func (s *pairReplayQuoteSource) generation() uint64 { return 1 }

func (s *pairReplayQuoteSource) nativeMid(id string, atMs int64) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.sealed[id]; ok {
		n := sort.Search(len(st.obs), func(i int) bool { return st.obs[i].RecvMs > atMs })
		if n == 0 {
			return 0, false
		}
		return pairReplayMid(st.obs[n-1].Bids[0].Px, st.obs[n-1].Asks[0].Px)
	}
	o, ok := s.latestPublic(id, atMs)
	if !ok || len(o.NativeBids) == 0 || len(o.NativeAsks) == 0 {
		return 0, false
	}
	return pairReplayMid(o.NativeBids[0].Px, o.NativeAsks[0].Px)
}

func pairReplayMid(bid, ask string) (float64, bool) {
	b, ok1 := new(big.Rat).SetString(bid)
	a, ok2 := new(big.Rat).SetString(ask)
	if !ok1 || !ok2 || b.Sign() <= 0 || a.Sign() <= 0 {
		return 0, false
	}
	mid, _ := new(big.Rat).Quo(new(big.Rat).Add(a, b), big.NewRat(2, 1)).Float64()
	return mid, basketFinitePositive(mid)
}

func (s *pairReplayQuoteSource) deliveries() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, list := range s.public {
		for _, o := range list {
			out = append(out, o.FirstReceiptMs)
		}
	}
	for _, st := range s.sealed {
		for _, o := range st.obs {
			out = append(out, o.RecvMs)
		}
	}
	return out
}

type pairReplayMarket struct {
	src   *pairReplayQuoteSource
	clock func() time.Time
}

func (m *pairReplayMarket) Events(ctx context.Context, key pairMemberKey, afterMs, throughMs int64) (pairVirtualMarketWindow, error) {
	if err := ctx.Err(); err != nil {
		return pairVirtualMarketWindow{}, err
	}
	if now := m.clock().UnixMilli(); throughMs > now {
		throughMs = now
	}
	s := m.src
	s.mu.Lock()
	member, ok := s.members[key.MemberID]
	bound := s.bound != "" && s.bindErr == nil
	s.mu.Unlock()
	if !bound || !ok {
		return pairVirtualMarketWindow{}, fmt.Errorf("the recorded market has no bound member %s", key.MemberID)
	}
	if want := (pairMemberKey{Partition: key.Partition, Provider: member.Provider, AccountKey: member.AccountKey, Instrument: member.Instrument, MemberID: member.MemberID}); key != want {
		return pairVirtualMarketWindow{}, fmt.Errorf("the recorded market member key %s is not the admitted key %s", key, want)
	}
	spec, _ := pairSourceSpecFor(member.Provider, member.Source)
	cov := pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
		FromMs: afterMs, ThroughMs: throughMs, Status: pairCoverageIncomplete, Gaps: []pairCoverageGap{}}
	w := pairVirtualMarketWindow{Events: []pairObservation{}}
	if afterMs >= throughMs {
		cov.ThroughMs = cov.FromMs
		w.Coverage = cov
		return w, nil
	}
	if member.Source != pairSourceSealedFeed {
		s.mu.Lock()
		for _, o := range s.public[member.MemberID] {
			if v := *o.VenueTimeMs; v > afterMs && v <= throughMs && o.FirstReceiptMs <= throughMs {
				w.Events = append(w.Events, pairCloneObservation(o))
			}
		}
		s.mu.Unlock()
		w.Coverage = cov
		return w, nil
	}
	s.mu.Lock()
	st := s.sealed[member.MemberID]
	need := s.need
	partition := s.partition
	s.mu.Unlock()
	feedNeed := contractBookNeed(need.WindowMs, need.Capacity, need.Depth)
	for _, rec := range st.obs {
		if rec.EventMs <= afterMs || rec.EventMs > throughMs || rec.RecvMs > throughMs {
			continue
		}
		book := st.freeze(feedNeed, rec.RecvMs)
		snap := &marketSnapshot{EvaluationID: fmt.Sprintf("replay/%s/seal/%d", s.runID, rec.RecvMs), ConfigGeneration: 1, Deadline: time.UnixMilli(rec.RecvMs).UTC(),
			books: map[feedObservationKey]*marketSnapshotBook{st.key: book}}
		p := snap.l2BookPayload(st.key, feedNeed)
		if !p.Available {
			return pairVirtualMarketWindow{}, fmt.Errorf("the recorded book window at %d is unavailable: %s", rec.RecvMs, p.Reason)
		}
		raw, err := pairCanonicalJSON(p)
		if err != nil {
			return pairVirtualMarketWindow{}, err
		}
		o, err := pairObservationFromPayload(member, partition, need, raw, 0, 1)
		if err != nil {
			return pairVirtualMarketWindow{}, fmt.Errorf("the recorded book window at %d does not derive: %w", rec.RecvMs, err)
		}
		if o.VenueTimeMs == nil || *o.VenueTimeMs != rec.EventMs || o.FirstReceiptMs != rec.RecvMs {
			return pairVirtualMarketWindow{}, fmt.Errorf("the recorded book window at %d derives venue %v and receipt %d, not its newest record", rec.RecvMs, o.VenueTimeMs, o.FirstReceiptMs)
		}
		w.Events = append(w.Events, o)
	}
	cov.PrefixComplete = st.doc.CoverageFromMs <= afterMs
	gaps := st.gaps(afterMs, throughMs)
	cov.Gaps = gaps
	if cov.PrefixComplete && len(gaps) == 0 {
		cov.Status = pairCoverageComplete
	}
	w.Coverage = cov
	return w, nil
}

func (st *pairReplaySealedStream) gaps(afterMs, throughMs int64) []pairCoverageGap {
	out := []pairCoverageGap{}
	if throughMs > st.doc.CoverageThroughMs {
		from := st.doc.CoverageThroughMs + 1
		if from < afterMs {
			from = afterMs
		}
		out = append(out, pairCoverageGap{FromMs: from, ThroughMs: throughMs, Reason: "end_of_input"})
	}
	for _, mk := range st.marks {
		if mk.AtMs >= afterMs && mk.AtMs <= throughMs {
			out = append(out, pairCoverageGap{FromMs: mk.AtMs, ThroughMs: mk.AtMs, Reason: "marker:" + mk.Reason})
		}
	}
	prev := afterMs
	for _, o := range st.obs {
		if o.RecvMs <= afterMs {
			prev = o.RecvMs
			continue
		}
		if o.RecvMs > throughMs {
			break
		}
		if o.RecvMs-prev > feedBookMaxGapMs {
			out = append(out, pairCoverageGap{FromMs: prev, ThroughMs: o.RecvMs, Reason: "receipt_gap_" + strconv.FormatInt(o.RecvMs-prev, 10) + "ms"})
		}
		prev = o.RecvMs
	}
	if end := throughMs; end <= st.doc.CoverageThroughMs && end-prev > feedBookMaxGapMs {
		out = append(out, pairCoverageGap{FromMs: prev, ThroughMs: end, Reason: "receipt_gap_" + strconv.FormatInt(end-prev, 10) + "ms"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromMs != out[j].FromMs {
			return out[i].FromMs < out[j].FromMs
		}
		return out[i].ThroughMs < out[j].ThroughMs
	})
	return out
}

func pairReplayFrame(bars []feedBar, intervalMs, atMs int64, keep int, openRow bool) []feedBar {
	k := sort.Search(len(bars), func(i int) bool { return bars[i].OpenMs+intervalMs > atMs })
	from := k - keep
	if from < 0 {
		from = 0
	}
	out := make([]feedBar, 0, k-from+1)
	for _, b := range bars[from:k] {
		b.RecvAt = time.UnixMilli(b.CloseMs + 1).UTC()
		out = append(out, b)
	}
	if openRow && k < len(bars) && bars[k].OpenMs <= atMs {
		f := bars[k]
		out = append(out, feedBar{OpenMs: f.OpenMs, CloseMs: f.CloseMs, HasClose: false, Open: f.Open, High: f.Open, Low: f.Open, Close: f.Open, Source: f.Source, RecvAt: time.UnixMilli(atMs).UTC()})
	}
	return out
}
