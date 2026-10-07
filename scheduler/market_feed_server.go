package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	feedSealSettleDelay      = 5 * time.Second
	feedSealPrepareBudget    = 20 * time.Second
	feedSealPublishGrace     = 2 * time.Second
	feedSealRetainPerCadence = 3
	feedSealOutcomeMemory    = 512
	feedServerConnDeadline   = 10 * time.Second
	feedServerMaxConns       = 64
	feedPendingRetryCap      = time.Second
	feedPendingRetryFloor    = 100 * time.Millisecond
	feedSocketFileMode       = 0o660
	feedSocketPathMaxBytes   = 100
)

type feedSeal struct {
	Key         int64
	Bytes       []byte
	Hash        string
	SealVersion int
	Generation  uint64
	SealedAt    time.Time
	Source      string
	KeysTotal   int
	KeysReady   int
	KeysStale   int
	Mids        int
	PrepareDur  time.Duration
}

type feedSealOutcome struct {
	Status string
	Detail string
}

type feedSealer struct {
	owner    *marketFeedOwner
	source   string
	instance string
	started  time.Time
	clock    func() time.Time
	logf     func(string, ...any)

	settle  time.Duration
	prepare time.Duration
	grace   time.Duration

	mu            sync.Mutex
	serving       bool
	firstDeadline int64
	cadences      map[int]int64
	reqs          cycleMarketRequirements
	coverage      feedRequirements
	ring          map[int64]*feedSeal
	outcomes      map[int64]feedSealOutcome
	outcomeOrder  []int64
	lastSeal      *feedSeal
	missedStreak  int
	wake          chan struct{}
	alertHook     func(string)
	prepareFn     feedPrepareFunc
	ledger        *feedRequestLedger
	budgetShort   bool
}

func newFeedSealer(owner *marketFeedOwner, source, instance string, clock func() time.Time, logf func(string, ...any)) *feedSealer {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &feedSealer{
		owner:    owner,
		source:   source,
		instance: instance,
		started:  clock().UTC(),
		clock:    clock,
		logf:     logf,
		settle:   feedSealSettleDelay,
		prepare:  feedSealPrepareBudget,
		grace:    feedSealPublishGrace,
		cadences: make(map[int]int64),
		ring:     make(map[int64]*feedSeal),
		outcomes: make(map[int64]feedSealOutcome),
		wake:     make(chan struct{}, 1),
	}
}

func (s *feedSealer) prepareFor(ctx context.Context, key int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
	if s.prepareFn != nil {
		return s.prepareFn(ctx, key, reqs, coverage)
	}
	prepareMarketSnapshot(ctx, s.owner, reqs)
	return feedPrepareReport{}
}

func (s *feedSealer) budgetLines(key int64, before feedBudgetTotals, rep feedPrepareReport) (feedBudgetTotals, []string) {
	if s.ledger == nil {
		return feedBudgetTotals{}, nil
	}
	delta := s.ledger.snapshot().since(before)
	st := s.ledger.status()
	line := fmt.Sprintf("[feed-budget] key=%d source=%s instance=%s requests=%d refused=%d by_reason=%s by_type=%s refused_by_reason=%s window_used=%d per_minute=%d startup_left=%d enforced=%t refreshed=%d refused_keys=%s failed_keys=%s",
		key, s.source, s.instance, delta.Total, delta.Refused, formatFeedCounts(delta.ByReason), formatFeedCounts(delta.ByType),
		formatFeedCounts(delta.RefusedByReason), st.WindowUsed, st.PerMinute, st.StartupLeft, st.Enforced, rep.Refreshed,
		joinFeedNames(rep.Refused), joinFeedNames(rep.Failed))
	return delta, []string{line}
}

func markUnrefreshedKeysNotReady(snap *marketSnapshot, refreshed map[marketFeedKey]bool, deadline int64) {
	if snap == nil {
		return
	}
	for k, entry := range snap.keys {
		if entry == nil || refreshed[k] || !entry.Readiness.Ready {
			continue
		}
		entry.Readiness.Ready = false
		entry.Readiness.Status = feedStatusNotDue
		entry.Readiness.Detail = fmt.Sprintf("not refreshed for deadline %d: no consumer cadence this feed loaded divides it", deadline)
	}
}

func joinFeedNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

func (s *feedSealer) budgetAlert(key int64, delta feedBudgetTotals, rep feedPrepareReport) {
	if s.ledger == nil || !s.ledger.status().Enforced {
		return
	}
	s.mu.Lock()
	was := s.budgetShort
	now := delta.Refused > 0
	s.budgetShort = now
	s.mu.Unlock()
	if s.alertHook == nil || was == now {
		return
	}
	if now {
		s.alertHook(fmt.Sprintf("**MARKET FEED REQUEST BUDGET EXHAUSTED** [%s] key %d: %d request(s) refused (%s); refused inputs: %s. Consumers hold entries on those inputs; protection continues on verified inputs (instance %s).",
			s.source, key, delta.Refused, formatFeedCounts(delta.RefusedByReason), joinFeedNames(rep.Refused), s.instance))
		return
	}
	s.alertHook(fmt.Sprintf("**MARKET FEED REQUEST BUDGET RECOVERED** [%s] key %d prepared with no refused request (instance %s).", s.source, key, s.instance))
}

func (s *feedSealer) now() time.Time {
	return s.clock().UTC()
}

func (s *feedSealer) hardDeadline(key int64) time.Time {
	return time.Unix(key, 0).UTC().Add(s.settle + s.prepare + s.grace)
}

func (s *feedSealer) sealAt(key int64) time.Time {
	return time.Unix(key, 0).UTC().Add(s.settle)
}

func (s *feedSealer) setGeneration(union feedRequirements, cadences []int, activateAfter time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = fullCycleRequirements(union)
	s.coverage = union
	act := activateAfter.UTC().Unix()
	next := make(map[int]int64, len(cadences))
	for _, c := range cadences {
		if c <= 0 {
			continue
		}
		if existing, ok := s.cadences[c]; ok {
			next[c] = existing
			continue
		}
		next[c] = act
	}
	s.cadences = next
	s.signal()
}

func (s *feedSealer) startServing(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	act := at.UTC().Unix()
	for c, existing := range s.cadences {
		if existing < act {
			s.cadences[c] = act
		}
	}
	s.serving = true
	s.firstDeadline = s.nextDeadlineLocked(act)
	s.signal()
}

func (s *feedSealer) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *feedSealer) nextDeadlineLocked(after int64) int64 {
	var best int64
	for c, act := range s.cadences {
		start := after
		if act > start {
			start = act
		}
		step := int64(c)
		d := (start/step + 1) * step
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

func (s *feedSealer) onScheduleLocked(key int64) bool {
	for c, act := range s.cadences {
		if key%int64(c) == 0 && key > act {
			return true
		}
	}
	return false
}

func (s *feedSealer) recordOutcomeLocked(key int64, status, detail string) {
	if _, seen := s.outcomes[key]; !seen {
		s.outcomeOrder = append(s.outcomeOrder, key)
	}
	s.outcomes[key] = feedSealOutcome{Status: status, Detail: detail}
	for len(s.outcomeOrder) > feedSealOutcomeMemory {
		delete(s.outcomes, s.outcomeOrder[0])
		s.outcomeOrder = s.outcomeOrder[1:]
	}
}

func (s *feedSealer) run(ctx context.Context) {
	var last int64
	for {
		s.mu.Lock()
		serving := s.serving
		after := last
		if after < s.firstDeadline-1 {
			after = s.firstDeadline - 1
		}
		next := int64(0)
		if serving {
			next = s.nextDeadlineLocked(after)
		}
		s.mu.Unlock()
		if next == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		wait := s.sealAt(next).Sub(s.now())
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.wake:
				timer.Stop()
				continue
			case <-timer.C:
			}
		}
		last = next
		s.sealOne(ctx, next)
	}
}

func (s *feedSealer) sealOne(ctx context.Context, key int64) {
	hard := s.hardDeadline(key)
	startedAt := s.now()
	if startedAt.After(s.sealAt(key).Add(s.prepare)) {
		s.mu.Lock()
		s.recordOutcomeLocked(key, feedWireStatusUnavailable, "seal preparation started after its budget ended")
		s.missedStreak++
		streak := s.missedStreak
		s.mu.Unlock()
		s.logf("[feed-seal] key=%d status=missed reason=%q instance=%s", key, "preparation started after its budget ended", s.instance)
		s.missedAlert(key, streak, "preparation started after its budget ended")
		return
	}
	s.mu.Lock()
	reqs := s.reqs
	coverage := s.coverage
	s.mu.Unlock()
	var before feedBudgetTotals
	if s.ledger != nil {
		before = s.ledger.snapshot()
	}
	prepCtx, cancel := context.WithDeadline(ctx, s.sealAt(key).Add(s.prepare))
	rep := s.prepareFor(prepCtx, key, reqs, coverage)
	cancel()
	delta, budgetLines := s.budgetLines(key, before, rep)
	for _, line := range budgetLines {
		s.logf("%s", line)
	}
	s.budgetAlert(key, delta, rep)
	frozenAt := s.owner.now()
	snap := freezeMarketSnapshot(s.owner, reqs, feedSealEvaluationID(key), frozenAt, time.Unix(key, 0).UTC())
	if rep.ReadyOnlyRefreshed {
		markUnrefreshedKeysNotReady(snap, rep.RefreshedKeys, key)
	}
	doc, err := feedSealDocFromSnapshot(snap, key, s.source, s.instance)
	var blob []byte
	var hash string
	if err == nil {
		blob, hash, err = encodeFeedSeal(doc)
	}
	prepDur := frozenAt.Sub(startedAt)
	if err != nil {
		s.mu.Lock()
		s.recordOutcomeLocked(key, feedWireStatusUnavailable, err.Error())
		s.missedStreak++
		streak := s.missedStreak
		s.mu.Unlock()
		s.logf("[feed-seal] key=%d status=failed reason=%q instance=%s", key, err.Error(), s.instance)
		s.missedAlert(key, streak, err.Error())
		return
	}
	seal := &feedSeal{
		Key:         key,
		Bytes:       blob,
		Hash:        hash,
		SealVersion: doc.V,
		Generation:  doc.Generation,
		SealedAt:    feedMsTime(doc.SealedAtMs),
		Source:      s.source,
		KeysTotal:   len(doc.Keys),
		Mids:        len(doc.Mids),
		PrepareDur:  prepDur,
	}
	for _, k := range doc.Keys {
		if k.Readiness.Ready {
			seal.KeysReady++
		}
		if k.Readiness.Stale {
			seal.KeysStale++
		}
	}
	s.mu.Lock()
	if s.now().After(hard) {
		s.recordOutcomeLocked(key, feedWireStatusUnavailable, "seal finished after its hard deadline")
		s.missedStreak++
		streak := s.missedStreak
		s.mu.Unlock()
		s.logf("[feed-seal] key=%d status=missed reason=%q instance=%s", key, "finished after its hard deadline", s.instance)
		s.missedAlert(key, streak, "finished after its hard deadline")
		return
	}
	if _, exists := s.ring[key]; exists {
		s.mu.Unlock()
		s.logf("[feed-seal] key=%d status=duplicate instance=%s: an immutable seal already exists; keeping it", key, s.instance)
		return
	}
	s.ring[key] = seal
	s.lastSeal = seal
	recovered := s.missedStreak > 0
	s.missedStreak = 0
	s.recordOutcomeLocked(key, feedWireStatusSealed, "")
	evicted := s.retainLocked()
	s.mu.Unlock()
	s.logf("[feed-seal] key=%d status=sealed source=%s instance=%s generation=%d hash=%s bytes=%d keys=%d ready=%d stale=%d mids=%d prepare_ms=%d sealed_at_ms=%d requests=%d refused=%d",
		key, seal.Source, s.instance, seal.Generation, seal.Hash, len(seal.Bytes), seal.KeysTotal, seal.KeysReady, seal.KeysStale, seal.Mids,
		prepDur.Milliseconds(), doc.SealedAtMs, delta.Total, delta.Refused)
	for _, ev := range evicted {
		s.logf("[feed-seal] key=%d status=evicted instance=%s", ev, s.instance)
	}
	for _, line := range formatFeedAlerts(s.owner.DrainAlerts()) {
		s.logf("%s", line)
	}
	if recovered && s.alertHook != nil {
		s.alertHook(fmt.Sprintf("**MARKET FEED SEALING RECOVERED** [%s] key %d sealed again (instance %s).", s.source, key, s.instance))
	}
}

func (s *feedSealer) missedAlert(key int64, streak int, detail string) {
	if streak != 1 || s.alertHook == nil {
		return
	}
	s.alertHook(fmt.Sprintf("**MARKET FEED SEAL MISSED** [%s] key %d: %s (instance %s). Consumers hold entries for this key; protection continues on verified inputs.",
		s.source, key, detail, s.instance))
}

func (s *feedSealer) retainLocked() []int64 {
	keep := make(map[int64]bool)
	keys := make([]int64, 0, len(s.ring))
	for k := range s.ring {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	for c := range s.cadences {
		kept := 0
		for _, k := range keys {
			if k%int64(c) != 0 {
				continue
			}
			keep[k] = true
			kept++
			if kept >= feedSealRetainPerCadence {
				break
			}
		}
	}
	if s.lastSeal != nil {
		keep[s.lastSeal.Key] = true
	}
	var evicted []int64
	for _, k := range keys {
		if keep[k] {
			continue
		}
		delete(s.ring, k)
		s.recordOutcomeLocked(k, feedWireStatusUnavailable, "evicted from the retention ring")
		evicted = append(evicted, k)
	}
	sort.Slice(evicted, func(i, j int) bool { return evicted[i] < evicted[j] })
	return evicted
}

func (s *feedSealer) lookup(key int64) feedWireHeader {
	h, _ := s.lookupWithBytes(key)
	return h
}

func (s *feedSealer) lookupWithBytes(key int64) (feedWireHeader, []byte) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.lookupLocked(key, now)
	if h.Status == feedWireStatusSealed {
		return h, s.ring[key].Bytes
	}
	return h, nil
}

func (s *feedSealer) lookupLocked(key int64, now time.Time) feedWireHeader {
	h := feedWireHeader{
		V:              feedWireVersion,
		Key:            key,
		Instance:       s.instance,
		Source:         s.source,
		SealVersion:    s.coverageSealVersionLocked(),
		PayloadVersion: marketSnapshotVersion,
		Generation:     s.owner.Generation(),
	}
	if seal, ok := s.ring[key]; ok {
		h.Status = feedWireStatusSealed
		h.SealVersion = seal.SealVersion
		h.Generation = seal.Generation
		h.SealedAtMs = feedTimeMs(seal.SealedAt)
		h.Hash = seal.Hash
		h.Bytes = len(seal.Bytes)
		return h
	}
	h.Status = feedWireStatusUnavailable
	if out, ok := s.outcomes[key]; ok {
		h.Detail = out.Detail
		return h
	}
	if !s.serving {
		h.Detail = "this feed instance is not serving yet"
		return h
	}
	if key < s.firstDeadline {
		h.Detail = fmt.Sprintf("key precedes this feed instance's first scheduled deadline %d", s.firstDeadline)
		return h
	}
	if !s.onScheduleLocked(key) {
		h.Detail = "key is not on this feed's deadline schedule"
		return h
	}
	hard := s.hardDeadline(key)
	if now.After(hard) {
		h.Detail = "no seal was stored before the key's hard deadline"
		return h
	}
	if next := s.nextDeadlineLocked(now.Unix()); next > 0 && key > next {
		h.Detail = fmt.Sprintf("key is after this feed's next scheduled deadline %d; check the consumer clock", next)
		return h
	}
	h.Status = feedWireStatusPending
	retry := s.sealAt(key).Sub(now)
	if retry > feedPendingRetryCap {
		retry = feedPendingRetryCap
	}
	if retry < feedPendingRetryFloor {
		retry = feedPendingRetryFloor
	}
	h.RetryAfterMs = retry.Milliseconds()
	h.GiveUpAtMs = hard.UnixMilli()
	return h
}

func (s *feedSealer) sealBytes(key int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seal, ok := s.ring[key]; ok {
		return seal.Bytes
	}
	return nil
}

func (s *feedSealer) coverageSealVersionLocked() int {
	return feedSealVersionFor(len(s.coverage.Observations) > 0, len(s.coverage.AccountingCoins) > 0)
}

func (s *feedSealer) coverageSealVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coverageSealVersionLocked()
}

func (s *feedSealer) describe() *feedDescribe {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &feedDescribe{
		StartedAtMs:      s.started.UnixMilli(),
		Serving:          s.serving,
		FirstDeadline:    s.firstDeadline,
		SettleMs:         s.settle.Milliseconds(),
		PrepareMs:        s.prepare.Milliseconds(),
		PublishGraceMs:   s.grace.Milliseconds(),
		RetainPerCadence: feedSealRetainPerCadence,
		RetainedKeys:     []int64{},
		Cadences:         []feedDescribeCadence{},
		Keys:             []feedDescribeKey{},
		MidCoins:         []string{},
		Funding:          []feedDescribeFunding{},
	}
	for k := range s.ring {
		d.RetainedKeys = append(d.RetainedKeys, k)
	}
	sort.Slice(d.RetainedKeys, func(i, j int) bool { return d.RetainedKeys[i] < d.RetainedKeys[j] })
	cadences := make([]int, 0, len(s.cadences))
	for c := range s.cadences {
		cadences = append(cadences, c)
	}
	sort.Ints(cadences)
	for _, c := range cadences {
		d.Cadences = append(d.Cadences, feedDescribeCadence{Seconds: c, FirstDeadline: s.nextDeadlineForCadenceLocked(c)})
	}
	published := s.owner.publishedCoverage()
	for _, key := range s.coverage.Order {
		if !published[key] {
			continue
		}
		d.Keys = append(d.Keys, feedDescribeKey{Host: key.Host, Namespace: key.Namespace, Symbol: key.Symbol, Timeframe: key.Timeframe, Required: s.coverage.Keys[key]})
	}
	d.MidCoins = append(d.MidCoins, s.coverage.MidCoins...)
	fundingCoins := make([]string, 0, len(s.coverage.Funding))
	for c := range s.coverage.Funding {
		fundingCoins = append(fundingCoins, c)
	}
	sort.Strings(fundingCoins)
	for _, c := range fundingCoins {
		need := s.coverage.Funding[c]
		d.Funding = append(d.Funding, feedDescribeFunding{Coin: c, Scalar: need.Scalar, Records: need.Records})
	}
	for _, key := range s.coverage.observationKeys() {
		d.Observations = append(d.Observations, feedDescribeObservation{
			Host: key.Host, Namespace: key.Namespace, Coin: key.Coin, Kind: key.Kind,
			Source: feedObservationSourceHLWS, WindowMs: s.coverage.Observations[key],
		})
	}
	if len(s.coverage.AccountingCoins) > 0 {
		d.AccountingFunding = append([]string(nil), s.coverage.AccountingCoins...)
		d.AccountingWindowMs = feedAccountingWindow.Milliseconds()
	}
	if s.lastSeal != nil {
		d.LastSealKey = s.lastSeal.Key
		d.LastSealHash = s.lastSeal.Hash
		d.LastSealReady = s.lastSeal.KeysReady
		d.LastSealKeysTotal = s.lastSeal.KeysTotal
	}
	return d
}

func (s *feedSealer) nextDeadlineForCadenceLocked(c int) int64 {
	act := s.cadences[c]
	start := act
	if s.firstDeadline-1 > start {
		start = s.firstDeadline - 1
	}
	step := int64(c)
	return (start/step + 1) * step
}

func (s *feedSealer) lastSealSnapshot() *feedSeal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSeal == nil {
		return nil
	}
	cp := *s.lastSeal
	cp.Bytes = nil
	return &cp
}

func (o *marketFeedOwner) publishedCoverage() map[marketFeedKey]bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make(map[marketFeedKey]bool, len(o.published))
	for k, v := range o.published {
		out[k] = v
	}
	return out
}

func fullCycleRequirements(req feedRequirements) cycleMarketRequirements {
	out := cycleMarketRequirements{
		Funding:      make(map[string]feedFundingNeed, len(req.Funding)),
		Observations: make(map[feedObservationKey]int64, len(req.Observations)),
	}
	for key, window := range req.Observations {
		out.Observations[key] = window
	}
	for _, key := range req.Order {
		out.Keys = append(out.Keys, cycleMarketRequirement{Key: key, Required: req.Keys[key]})
	}
	out.Coins = append(out.Coins, req.MidCoins...)
	sort.Strings(out.Coins)
	for coin, need := range req.Funding {
		out.Funding[coin] = need
	}
	out.AccountingCoins = append(out.AccountingCoins, req.AccountingCoins...)
	return out
}

type feedUnixServer struct {
	sealer   *feedSealer
	listener net.Listener
	path     string
	sem      chan struct{}
	wg       sync.WaitGroup
	closed   atomic.Bool
	logf     func(string, ...any)
}

func listenFeedSocket(path string, sealer *feedSealer, logf func(string, ...any)) (*feedUnixServer, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, feedSocketFileMode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return &feedUnixServer{
		sealer:   sealer,
		listener: ln,
		path:     path,
		sem:      make(chan struct{}, feedServerMaxConns),
		logf:     logf,
	}, nil
}

func (srv *feedUnixServer) serve() {
	for {
		conn, err := srv.listener.Accept()
		if err != nil {
			if srv.closed.Load() {
				return
			}
			srv.logf("[feed-server] accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case srv.sem <- struct{}{}:
		default:
			srv.logf("[feed-server] %d connections already open; refusing one", feedServerMaxConns)
			conn.Close()
			continue
		}
		srv.wg.Add(1)
		go func() {
			defer srv.wg.Done()
			defer func() { <-srv.sem }()
			srv.handle(conn)
		}()
	}
}

func (srv *feedUnixServer) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(feedServerConnDeadline))
	blob, err := readFeedFrame(conn, feedWireMaxRequestBytes)
	if err != nil {
		srv.replyError(conn, fmt.Sprintf("bad request frame: %v", err))
		return
	}
	var req feedWireRequest
	if err := decodeFeedStrict(blob, &req); err != nil {
		srv.replyError(conn, fmt.Sprintf("bad request: %v", err))
		return
	}
	if req.V != feedWireVersion {
		srv.replyError(conn, fmt.Sprintf("wire version %d is not supported (this feed speaks %d)", req.V, feedWireVersion))
		return
	}
	switch req.Op {
	case feedWireOpDescribe:
		d := srv.sealer.describe()
		h := feedWireHeader{
			V:              feedWireVersion,
			Status:         feedWireStatusDescribe,
			Instance:       srv.sealer.instance,
			Source:         srv.sealer.source,
			Generation:     srv.sealer.owner.Generation(),
			SealVersion:    feedSealVersionFor(len(d.Observations) > 0, len(d.AccountingFunding) > 0),
			PayloadVersion: marketSnapshotVersion,
			Describe:       d,
		}
		_ = writeFeedJSONFrame(conn, h)
	case feedWireOpSnapshot:
		if req.Key <= 0 {
			srv.replyError(conn, "snapshot request needs a positive key")
			return
		}
		h, body := srv.sealer.lookupWithBytes(req.Key)
		if err := writeFeedJSONFrame(conn, h); err != nil {
			return
		}
		if body != nil {
			_ = writeFeedFrame(conn, body)
		}
	default:
		srv.replyError(conn, fmt.Sprintf("unknown op %q", req.Op))
	}
}

func (srv *feedUnixServer) replyError(conn net.Conn, detail string) {
	_ = writeFeedJSONFrame(conn, feedWireHeader{
		V:              feedWireVersion,
		Status:         feedWireStatusError,
		Detail:         detail,
		Instance:       srv.sealer.instance,
		Source:         srv.sealer.source,
		SealVersion:    srv.sealer.coverageSealVersion(),
		PayloadVersion: marketSnapshotVersion,
	})
}

func (srv *feedUnixServer) close() {
	if srv.closed.Swap(true) {
		return
	}
	srv.listener.Close()
	srv.wg.Wait()
	if err := os.Remove(srv.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		srv.logf("[feed-server] remove %s: %v", srv.path, err)
	}
}

func feedSocketPathErrors(field, path string) []string {
	var errs []string
	trimmed := strings.TrimSpace(path)
	switch {
	case trimmed == "":
		errs = append(errs, fmt.Sprintf("%s is empty", field))
	case !strings.HasPrefix(trimmed, "/"):
		errs = append(errs, fmt.Sprintf("%s %q must be an absolute path", field, path))
	case cleanFeedPath(trimmed) != trimmed:
		errs = append(errs, fmt.Sprintf("%s %q is not a clean path (want %q)", field, path, cleanFeedPath(trimmed)))
	case len(trimmed) > feedSocketPathMaxBytes:
		errs = append(errs, fmt.Sprintf("%s %q is %d bytes, over the %d-byte socket path limit", field, path, len(trimmed), feedSocketPathMaxBytes))
	}
	return errs
}
