package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	feedObservationPerpPremium = "perp_premium"
	feedPremiumSourceHLWS      = "hyperliquid_ws_activeAssetCtx+bbo"
	feedPremiumContract        = "perp-premium-quote-oracle/1639-v1"
	feedPremiumTimeBasis       = "receipt_cutoff_latest_component"
	feedPremiumOracleTimeBasis = "receipt_only_no_source_time"
	feedPremiumSampleMs        = int64(60_000)
	feedPremiumMaxFutureSkewMs = int64(5_000)
	feedPremiumMaxWindowMs     = int64(26 * 3_600_000)
	feedPremiumRetainSlackMs   = int64(10 * 60_000)
	feedPremiumMaxSamples      = int((feedPremiumMaxWindowMs + feedPremiumRetainSlackMs) / feedPremiumSampleMs)
	feedPremiumStaleAfterMs    = 2 * feedPremiumSampleMs

	feedPremiumStatusSampled = "sampled"
	feedPremiumStatusMissing = "missing"
	feedPremiumStatusInvalid = "invalid"

	feedPremiumOutcomeAccepted   = "accepted"
	feedPremiumOutcomeOneSided   = "one_sided"
	feedPremiumOutcomeDuplicate  = "duplicate"
	feedPremiumOutcomeConflict   = "conflict"
	feedPremiumOutcomeOutOfOrder = "out_of_order"
	feedPremiumOutcomeRejected   = "rejected"

	feedPremiumReasonNoQuote      = "no_quote"
	feedPremiumReasonNoContext    = "no_context"
	feedPremiumReasonOneSided     = "one_sided_book"
	feedPremiumReasonDisconnected = "disconnected"
	feedPremiumReasonMalformed    = "malformed"
	feedPremiumReasonConflict     = "conflicting_duplicate"
	feedPremiumReasonOutOfOrder   = "out_of_order"
	feedPremiumReasonFuture       = "future_event"
	feedPremiumReasonClock        = "clock_regression"
	feedPremiumReasonCrossed      = "crossed_book"

	perpetualPremiumStrategyName      = "perpetual_premium_reversion"
	perpetualPremiumDefaultLookback   = 60
	perpetualPremiumMaxLookback       = 720
	perpetualPremiumDefaultIntervalMs = int64(60_000)
)

func premiumKeyFor(coin string) feedObservationKey {
	return feedObservationKey{Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, Kind: feedObservationPerpPremium}
}

type feedPremiumQuote struct {
	BidPx   string
	BidSz   string
	BidN    int64
	AskPx   string
	AskSz   string
	AskN    int64
	EventMs int64
	RecvMs  int64
	Session uint64
	Seq     uint64
}

type feedPremiumContext struct {
	OraclePx string
	MarkPx   string
	MidPx    string
	RecvMs   int64
	Session  uint64
	Seq      uint64
}

type feedPremiumSample struct {
	T           int64
	Status      string
	Reason      string
	Quote       *feedPremiumQuote
	Context     *feedPremiumContext
	FinalizedMs int64
	InvalidMs   int64
	InvalidWhy  string
}

type feedPremiumStats struct {
	QuoteFrames     uint64
	QuoteAccepted   uint64
	QuoteOneSided   uint64
	QuoteDuplicates uint64
	QuoteConflicts  uint64
	QuoteOutOfOrder uint64
	CtxFrames       uint64
	CtxAccepted     uint64
	Rejected        uint64
	Finalized       uint64
	Sampled         uint64
	Missing         uint64
	Invalid         uint64
	LastReject      string
}

type feedPremiumState struct {
	Key      feedObservationKey
	WindowMs int64
	Samples  []*feedPremiumSample
	Gaps     []feedObservationGap
	NextMs   int64
	Stats    feedPremiumStats

	quote          *feedPremiumQuote
	quoteMissing   string
	ctx            *feedPremiumContext
	ctxMissing     string
	lastQuoteMs    int64
	lastQuoteHash  [32]byte
	hasQuoteHash   bool
	poisonedMs     int64
	quoteSeq       uint64
	ctxSeq         uint64
	lastRecvMs     int64
	pendingInvalid string
}

func newFeedPremiumState(key feedObservationKey, windowMs int64) *feedPremiumState {
	return &feedPremiumState{Key: key, WindowMs: windowMs, quoteMissing: feedPremiumReasonNoQuote, ctxMissing: feedPremiumReasonNoContext}
}

func (s *feedPremiumState) raiseWindow(windowMs int64) {
	if windowMs > s.WindowMs {
		s.WindowMs = windowMs
	}
}

func feedPremiumGridAtOrAfter(ms int64) int64 {
	q := ms / feedPremiumSampleMs
	if ms%feedPremiumSampleMs != 0 && ms > 0 {
		q++
	}
	return q * feedPremiumSampleMs
}

func (s *feedPremiumState) openGap() *feedObservationGap {
	if n := len(s.Gaps); n > 0 && s.Gaps[n-1].EndMs == 0 {
		return &s.Gaps[n-1]
	}
	return nil
}

type feedPremiumEvent struct {
	Kind   string
	Fields map[string]any
}

type feedPremiumEvents []feedPremiumEvent

func (e *feedPremiumEvents) add(kind string, fields map[string]any) {
	*e = append(*e, feedPremiumEvent{Kind: kind, Fields: fields})
}

func premiumQuoteFields(q *feedPremiumQuote) map[string]any {
	if q == nil {
		return nil
	}
	return map[string]any{
		"bid": q.BidPx, "bid_sz": q.BidSz, "bid_n": q.BidN, "ask": q.AskPx, "ask_sz": q.AskSz, "ask_n": q.AskN,
		"event_ms": q.EventMs, "recv_ms": q.RecvMs, "session": q.Session, "seq": q.Seq,
	}
}

func premiumContextFields(c *feedPremiumContext) map[string]any {
	if c == nil {
		return nil
	}
	var mid any
	if c.MidPx != "" {
		mid = c.MidPx
	}
	return map[string]any{
		"oracle": c.OraclePx, "mark": c.MarkPx, "mid": mid, "oracle_event_ms": nil,
		"recv_ms": c.RecvMs, "session": c.Session, "seq": c.Seq,
	}
}

func premiumSampleEventFields(s *feedPremiumState, smp *feedPremiumSample) map[string]any {
	return map[string]any{
		"coin": s.Key.Coin, "kind": feedObservationPerpPremium, "t": smp.T, "status": smp.Status, "reason": smp.Reason,
		"quote": premiumQuoteFields(smp.Quote), "context": premiumContextFields(smp.Context),
		"finalized_ms": smp.FinalizedMs, "recv_ms": smp.FinalizedMs,
	}
}

func (s *feedPremiumState) initAt(recvMs int64) {
	if s.NextMs == 0 {
		s.NextMs = feedPremiumGridAtOrAfter(recvMs)
	}
}

func (s *feedPremiumState) advance(nowMs int64, ev *feedPremiumEvents) int {
	if s.NextMs == 0 {
		return 0
	}
	if floor := feedPremiumGridAtOrAfter(nowMs - feedPremiumMaxWindowMs - feedPremiumRetainSlackMs); s.NextMs < floor {
		s.NextMs = floor
	}
	n := 0
	for steps := 0; steps <= feedPremiumMaxSamples && s.NextMs < nowMs; steps++ {
		smp := &feedPremiumSample{T: s.NextMs, FinalizedMs: nowMs}
		if s.quote != nil {
			cp := *s.quote
			smp.Quote = &cp
		}
		if s.ctx != nil {
			cp := *s.ctx
			smp.Context = &cp
		}
		switch {
		case s.pendingInvalid != "":
			smp.Status, smp.Reason = feedPremiumStatusInvalid, s.pendingInvalid
			s.Stats.Invalid++
		case s.quote == nil:
			smp.Status, smp.Reason = feedPremiumStatusMissing, s.quoteMissing
			s.Stats.Missing++
		case s.ctx == nil:
			smp.Status, smp.Reason = feedPremiumStatusMissing, s.ctxMissing
			s.Stats.Missing++
		default:
			smp.Status = feedPremiumStatusSampled
			s.Stats.Sampled++
		}
		s.pendingInvalid = ""
		s.Stats.Finalized++
		s.Samples = append(s.Samples, smp)
		ev.add("psample", premiumSampleEventFields(s, smp))
		s.NextMs += feedPremiumSampleMs
		n++
	}
	s.trim(nowMs)
	return n
}

func (s *feedPremiumState) trim(nowMs int64) {
	floor := nowMs - s.WindowMs - feedPremiumRetainSlackMs
	cut := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T >= floor })
	if over := len(s.Samples) - cut - feedPremiumMaxSamples; over > 0 {
		cut += over
	}
	if cut > 0 {
		s.Samples = append([]*feedPremiumSample(nil), s.Samples[cut:]...)
	}
	kept := s.Gaps[:0]
	for _, g := range s.Gaps {
		if g.EndMs != 0 && g.EndMs < floor {
			continue
		}
		kept = append(kept, g)
	}
	s.Gaps = kept
}

func (s *feedPremiumState) invalidateOpen(atMs int64, reason string) {
	if s.pendingInvalid == "" {
		s.pendingInvalid = reason
	}
	if n := len(s.Gaps); n == 0 || s.Gaps[n-1].Reason != reason || s.Gaps[n-1].DetectedMs != atMs {
		if g := s.openGap(); g != nil {
			g.EndMs = atMs
		}
		s.Gaps = append(s.Gaps, feedObservationGap{StartMs: atMs, EndMs: atMs, DetectedMs: atMs, Reason: reason})
	}
}

func (s *feedPremiumState) reject(atMs int64, reason, detail string, clearQuote, clearCtx bool) {
	s.Stats.Rejected++
	s.Stats.LastReject = detail
	if clearQuote {
		s.quote = nil
		s.quoteMissing = reason
	}
	if clearCtx {
		s.ctx = nil
		s.ctxMissing = reason
	}
	s.invalidateOpen(atMs, reason)
}

func (s *feedPremiumState) closeGapOnData(recvMs int64) {
	if g := s.openGap(); g != nil {
		g.EndMs = recvMs
	}
}

func (s *feedPremiumState) markDisconnected(atMs int64, ev *feedPremiumEvents) {
	if s.NextMs == 0 {
		return
	}
	if atMs >= s.lastRecvMs {
		s.advance(atMs, ev)
	}
	s.quote, s.quoteMissing = nil, feedPremiumReasonDisconnected
	s.ctx, s.ctxMissing = nil, feedPremiumReasonDisconnected
	if s.openGap() == nil {
		start := s.lastRecvMs
		if start == 0 || start > atMs {
			start = atMs
		}
		s.Gaps = append(s.Gaps, feedObservationGap{StartMs: start, DetectedMs: atMs, Reason: feedPremiumReasonDisconnected})
	}
}

type feedPremiumOutcome struct {
	Outcome string `json:"o"`
	Reason  string `json:"why,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
}

type feedSocketBBOLevel struct {
	Px string      `json:"px"`
	Sz string      `json:"sz"`
	N  json.Number `json:"n"`
}

type feedSocketBBO struct {
	Coin string            `json:"coin"`
	Time json.Number       `json:"time"`
	BBO  []json.RawMessage `json:"bbo"`
}

type feedPremiumQuoteParse struct {
	Coin     string
	EventMs  int64
	OneSided bool
	Quote    feedPremiumQuote
	Digest   [32]byte
	Err      error
	Crossed  bool
}

func parsePositiveFeedDecimal(raw, what string) (string, error) {
	c, err := canonicalFeedDecimal(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	v, err := strconv.ParseFloat(c, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return "", fmt.Errorf("%s %q is not a positive finite decimal", what, raw)
	}
	return c, nil
}

func parseFeedBBOLevel(raw json.RawMessage, side string) (*feedSocketBBOLevel, string, string, int64, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, "", "", 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var lvl feedSocketBBOLevel
	if err := dec.Decode(&lvl); err != nil {
		return nil, "", "", 0, fmt.Errorf("bbo %s level: %w", side, err)
	}
	px, err := parsePositiveFeedDecimal(lvl.Px, "bbo "+side+" px")
	if err != nil {
		return nil, "", "", 0, err
	}
	sz, err := parsePositiveFeedDecimal(lvl.Sz, "bbo "+side+" sz")
	if err != nil {
		return nil, "", "", 0, err
	}
	n, err := strconv.ParseInt(lvl.N.String(), 10, 64)
	if err != nil || n < 0 {
		return nil, "", "", 0, fmt.Errorf("bbo %s order count %q is not a nonnegative integer", side, lvl.N.String())
	}
	return &lvl, px, sz, n, nil
}

func parseFeedBBO(data json.RawMessage) feedPremiumQuoteParse {
	var out feedPremiumQuoteParse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var msg feedSocketBBO
	if err := dec.Decode(&msg); err != nil {
		out.Err = fmt.Errorf("decode bbo: %w", err)
		return out
	}
	out.Coin = strings.TrimSpace(msg.Coin)
	if out.Coin == "" {
		out.Err = errors.New("bbo carries no coin")
		return out
	}
	t, err := strconv.ParseInt(msg.Time.String(), 10, 64)
	if err != nil || t <= 0 {
		out.Err = fmt.Errorf("bbo time %q is not a positive millisecond timestamp", msg.Time.String())
		return out
	}
	out.EventMs = t
	if len(msg.BBO) != 2 {
		out.Err = fmt.Errorf("bbo carries %d levels, want [bid, ask]", len(msg.BBO))
		return out
	}
	bid, bidPx, bidSz, bidN, err := parseFeedBBOLevel(msg.BBO[0], "bid")
	if err != nil {
		out.Err = err
		return out
	}
	ask, askPx, askSz, askN, err := parseFeedBBOLevel(msg.BBO[1], "ask")
	if err != nil {
		out.Err = err
		return out
	}
	out.Digest = sha256.Sum256([]byte(strings.Join([]string{out.Coin, strconv.FormatInt(t, 10), bidPx, bidSz, strconv.FormatInt(bidN, 10), askPx, askSz, strconv.FormatInt(askN, 10)}, "|")))
	if bid == nil || ask == nil {
		out.OneSided = true
		return out
	}
	bv, _ := strconv.ParseFloat(bidPx, 64)
	av, _ := strconv.ParseFloat(askPx, 64)
	if bv >= av {
		out.Crossed = true
		out.Err = fmt.Errorf("bbo bid %s is not below ask %s", bidPx, askPx)
		return out
	}
	out.Quote = feedPremiumQuote{BidPx: bidPx, BidSz: bidSz, BidN: bidN, AskPx: askPx, AskSz: askSz, AskN: askN, EventMs: t}
	return out
}

type feedPremiumContextParse struct {
	Coin    string
	Context feedPremiumContext
	Err     error
}

func parseFeedPremiumContext(data json.RawMessage) feedPremiumContextParse {
	var out feedPremiumContextParse
	var msg feedSocketAssetCtx
	if err := json.Unmarshal(data, &msg); err != nil {
		out.Err = fmt.Errorf("decode activeAssetCtx: %w", err)
		return out
	}
	out.Coin = strings.TrimSpace(msg.Coin)
	if out.Coin == "" {
		out.Err = errors.New("activeAssetCtx carries no coin")
		return out
	}
	text := func(name string, nullable bool) (string, error) {
		raw, ok := msg.Ctx[name]
		if !ok {
			return "", fmt.Errorf("activeAssetCtx for %s carries no %s", out.Coin, name)
		}
		if strings.TrimSpace(string(raw)) == "null" {
			if nullable {
				return "", nil
			}
			return "", fmt.Errorf("activeAssetCtx %s for %s is null", name, out.Coin)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("activeAssetCtx %s for %s is not a decimal string: %s", name, out.Coin, string(raw))
		}
		return parsePositiveFeedDecimal(s, "activeAssetCtx "+name)
	}
	oracle, err := text("oraclePx", false)
	if err != nil {
		out.Err = err
		return out
	}
	mark, err := text("markPx", false)
	if err != nil {
		out.Err = err
		return out
	}
	mid, err := text("midPx", true)
	if err != nil {
		out.Err = err
		return out
	}
	out.Context = feedPremiumContext{OraclePx: oracle, MarkPx: mark, MidPx: mid}
	return out
}

func (s *feedPremiumState) ingestQuote(p feedPremiumQuoteParse, recvMs int64, session uint64, ev *feedPremiumEvents) feedPremiumOutcome {
	s.Stats.QuoteFrames++
	s.initAt(recvMs)
	if recvMs < s.lastRecvMs {
		s.reject(recvMs, feedPremiumReasonClock, fmt.Sprintf("clock_regression: receipt %d is before the previous receipt %d", recvMs, s.lastRecvMs), true, true)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeRejected, Reason: feedPremiumReasonClock}
	}
	s.lastRecvMs = recvMs
	s.advance(recvMs, ev)
	if p.Err != nil {
		reason := feedPremiumReasonMalformed
		if p.Crossed {
			reason = feedPremiumReasonCrossed
		}
		s.reject(recvMs, reason, p.Err.Error(), true, false)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeRejected, Reason: p.Err.Error()}
	}
	if p.EventMs > recvMs+feedPremiumMaxFutureSkewMs {
		s.reject(recvMs, feedPremiumReasonFuture, fmt.Sprintf("bbo time %d is over %dms after its receipt %d", p.EventMs, feedPremiumMaxFutureSkewMs, recvMs), true, false)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeRejected, Reason: feedPremiumReasonFuture}
	}
	if s.hasQuoteHash && p.EventMs < s.lastQuoteMs {
		s.Stats.QuoteOutOfOrder++
		s.reject(recvMs, feedPremiumReasonOutOfOrder, fmt.Sprintf("bbo time %d is before the previous bbo time %d", p.EventMs, s.lastQuoteMs), true, false)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeOutOfOrder, Reason: feedPremiumReasonOutOfOrder}
	}
	if s.poisonedMs != 0 && p.EventMs == s.poisonedMs {
		s.Stats.QuoteConflicts++
		s.reject(recvMs, feedPremiumReasonConflict, fmt.Sprintf("bbo time %d already carried conflicting content", p.EventMs), true, false)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeConflict, Reason: feedPremiumReasonConflict}
	}
	if s.hasQuoteHash && p.EventMs == s.lastQuoteMs {
		if p.Digest == s.lastQuoteHash {
			if s.quote != nil || s.quoteMissing == feedPremiumReasonOneSided {
				s.Stats.QuoteDuplicates++
				return feedPremiumOutcome{Outcome: feedPremiumOutcomeDuplicate}
			}
		} else {
			s.Stats.QuoteConflicts++
			s.poisonedMs = p.EventMs
			s.reject(recvMs, feedPremiumReasonConflict, fmt.Sprintf("bbo time %d arrived twice with different content", p.EventMs), true, false)
			s.invalidateQuoteSamples(p.EventMs, recvMs, ev)
			return feedPremiumOutcome{Outcome: feedPremiumOutcomeConflict, Reason: feedPremiumReasonConflict}
		}
	}
	s.lastQuoteMs, s.lastQuoteHash, s.hasQuoteHash = p.EventMs, p.Digest, true
	s.closeGapOnData(recvMs)
	if p.OneSided {
		s.Stats.QuoteOneSided++
		s.quote, s.quoteMissing = nil, feedPremiumReasonOneSided
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeOneSided, Reason: feedPremiumReasonOneSided}
	}
	s.quoteSeq++
	q := p.Quote
	q.RecvMs, q.Session, q.Seq = recvMs, session, s.quoteSeq
	s.quote = &q
	s.Stats.QuoteAccepted++
	return feedPremiumOutcome{Outcome: feedPremiumOutcomeAccepted, Seq: q.Seq}
}

func (s *feedPremiumState) invalidateQuoteSamples(eventMs, atMs int64, ev *feedPremiumEvents) {
	for i := len(s.Samples) - 1; i >= 0; i-- {
		smp := s.Samples[i]
		if smp.Quote == nil || smp.Quote.EventMs != eventMs {
			break
		}
		if smp.InvalidMs != 0 {
			continue
		}
		smp.InvalidMs, smp.InvalidWhy = atMs, feedPremiumReasonConflict
		ev.add("pinvalidate", map[string]any{"coin": s.Key.Coin, "kind": feedObservationPerpPremium, "t": smp.T, "reason": feedPremiumReasonConflict, "recv_ms": atMs})
	}
}

func (s *feedPremiumState) ingestContext(p feedPremiumContextParse, recvMs int64, session uint64, ev *feedPremiumEvents) feedPremiumOutcome {
	s.Stats.CtxFrames++
	s.initAt(recvMs)
	if recvMs < s.lastRecvMs {
		s.reject(recvMs, feedPremiumReasonClock, fmt.Sprintf("clock_regression: receipt %d is before the previous receipt %d", recvMs, s.lastRecvMs), true, true)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeRejected, Reason: feedPremiumReasonClock}
	}
	s.lastRecvMs = recvMs
	s.advance(recvMs, ev)
	if p.Err != nil {
		s.reject(recvMs, feedPremiumReasonMalformed, p.Err.Error(), false, true)
		return feedPremiumOutcome{Outcome: feedPremiumOutcomeRejected, Reason: p.Err.Error()}
	}
	s.closeGapOnData(recvMs)
	s.ctxSeq++
	c := p.Context
	c.RecvMs, c.Session, c.Seq = recvMs, session, s.ctxSeq
	s.ctx = &c
	s.Stats.CtxAccepted++
	return feedPremiumOutcome{Outcome: feedPremiumOutcomeAccepted, Seq: c.Seq}
}

func (o *marketFeedOwner) emitPremiumEventsLocked(ev feedPremiumEvents) {
	if o.recorder == nil {
		return
	}
	for _, e := range ev {
		recv, _ := e.Fields["recv_ms"].(int64)
		o.recorder.offer(observationRecord{Kind: e.Kind, RecvMs: recv, Session: o.obsSession, Extra: e.Fields})
	}
}

func (o *marketFeedOwner) PremiumCoins() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make([]string, 0, len(o.premiums))
	for k := range o.premiums {
		out = append(out, k.Coin)
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) IngestPremiumQuote(data json.RawMessage, raw []byte) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.premiums) == 0 {
		return
	}
	recvMs := o.now().UnixMilli()
	p := parseFeedBBO(data)
	if p.Coin == "" {
		o.rejectAllPremiumsLocked(recvMs, "bbo", raw, p.Err)
		return
	}
	st := o.premiums[premiumKeyFor(p.Coin)]
	if st == nil {
		return
	}
	var ev feedPremiumEvents
	out := st.ingestQuote(p, recvMs, o.obsSession, &ev)
	o.recordPremiumFrameLocked("bbo", p.Coin, recvMs, raw, out)
	o.emitPremiumEventsLocked(ev)
	if out.Outcome == feedPremiumOutcomeRejected || out.Outcome == feedPremiumOutcomeConflict || out.Outcome == feedPremiumOutcomeOutOfOrder {
		o.logf("[feed] %s: %s bbo record: %s", st.Key.PayloadID(), out.Outcome, out.Reason)
	}
}

func (o *marketFeedOwner) IngestPremiumContext(data json.RawMessage, raw []byte) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.premiums) == 0 {
		return
	}
	recvMs := o.now().UnixMilli()
	p := parseFeedPremiumContext(data)
	if p.Coin == "" {
		o.rejectAllPremiumsLocked(recvMs, "pctx", raw, p.Err)
		return
	}
	st := o.premiums[premiumKeyFor(p.Coin)]
	if st == nil {
		return
	}
	var ev feedPremiumEvents
	out := st.ingestContext(p, recvMs, o.obsSession, &ev)
	o.recordPremiumFrameLocked("pctx", p.Coin, recvMs, raw, out)
	o.emitPremiumEventsLocked(ev)
	if out.Outcome == feedPremiumOutcomeRejected {
		o.logf("[feed] %s: rejected an activeAssetCtx record: %s", st.Key.PayloadID(), out.Reason)
	}
}

func (o *marketFeedOwner) IngestPremiumUnparsable(raw []byte) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.premiums) == 0 {
		return
	}
	o.rejectAllPremiumsLocked(o.now().UnixMilli(), "pbad", raw, errors.New("socket frame is not a JSON envelope"))
}

func (o *marketFeedOwner) rejectAllPremiumsLocked(recvMs int64, kind string, raw []byte, err error) {
	reason := "frame names no coin"
	if err != nil {
		reason = err.Error()
	}
	var ev feedPremiumEvents
	for _, k := range sortedPremiumKeys(o.premiums) {
		st := o.premiums[k]
		if st.NextMs == 0 {
			continue
		}
		if recvMs < st.lastRecvMs {
			st.reject(recvMs, feedPremiumReasonClock, fmt.Sprintf("clock_regression: receipt %d is before the previous receipt %d", recvMs, st.lastRecvMs), true, true)
			continue
		}
		st.lastRecvMs = recvMs
		st.advance(recvMs, &ev)
		st.reject(recvMs, feedPremiumReasonMalformed, reason, kind != "pctx", kind != "bbo")
	}
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: kind, RecvMs: recvMs, Session: o.obsSession, Extra: map[string]any{
			"coin": "", "raw": string(raw), "recv_ms": recvMs, "o": feedPremiumOutcomeRejected, "why": reason,
		}})
	}
	o.emitPremiumEventsLocked(ev)
	o.logf("[feed] rejected a premium input frame that names no coin: %s", reason)
}

func (o *marketFeedOwner) recordPremiumFrameLocked(kind, coin string, recvMs int64, raw []byte, out feedPremiumOutcome) {
	if o.recorder == nil {
		return
	}
	fields := map[string]any{"coin": coin, "raw": string(raw), "recv_ms": recvMs, "o": out.Outcome}
	if out.Reason != "" {
		fields["why"] = out.Reason
	}
	if out.Seq != 0 {
		fields["seq"] = out.Seq
	}
	o.recorder.offer(observationRecord{Kind: kind, RecvMs: recvMs, Session: o.obsSession, Extra: fields})
}

func sortedPremiumKeys(m map[feedObservationKey]*feedPremiumState) []feedObservationKey {
	keys := make([]feedObservationKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	return keys
}

func (o *marketFeedOwner) AdvancePremiums(now time.Time) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.tickPremiumsLocked(now.UTC().UnixMilli())
}

func (o *marketFeedOwner) tickPremiumsLocked(nowMs int64) {
	var ev feedPremiumEvents
	for _, k := range sortedPremiumKeys(o.premiums) {
		st := o.premiums[k]
		if st.NextMs == 0 || nowMs < st.lastRecvMs {
			continue
		}
		st.advance(nowMs, &ev)
	}
	if len(ev) > 0 && o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "ptick", RecvMs: nowMs, Session: o.obsSession, Extra: map[string]any{"recv_ms": nowMs}})
	}
	o.emitPremiumEventsLocked(ev)
}

func (o *marketFeedOwner) markPremiumsDisconnectedLocked(atMs int64) {
	var ev feedPremiumEvents
	for _, k := range sortedPremiumKeys(o.premiums) {
		o.premiums[k].markDisconnected(atMs, &ev)
	}
	o.emitPremiumEventsLocked(ev)
}

func (o *marketFeedOwner) trackPremiums(coins []string, windowMs int64) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	needs := make(map[feedObservationKey]int64, len(coins))
	for _, c := range coins {
		needs[premiumKeyFor(c)] = windowMs
	}
	o.applyObservationNeedsLocked(needs)
	o.subVersion++
}

type feedPremiumReadiness struct {
	Key          feedObservationKey
	Status       string
	Detail       string
	Ready        bool
	Samples      int
	SampledRun   int
	CoveredMs    int64
	WindowMs     int64
	LastSampleMs int64
	LastRecvMs   int64
	OpenGap      bool
	Stats        feedPremiumStats
	SourceDetail string
}

func premiumReadiness(s *feedPremiumState, nowMs int64) feedPremiumReadiness {
	out := feedPremiumReadiness{
		Key: s.Key, Samples: len(s.Samples), WindowMs: s.WindowMs, LastRecvMs: s.lastRecvMs, Stats: s.Stats,
		SourceDetail: feedPremiumSourceHLWS + "/" + feedPremiumContract + "/" + feedPremiumTimeBasis,
	}
	var session uint64
	for i := len(s.Samples) - 1; i >= 0; i-- {
		smp := s.Samples[i]
		if smp.Status != feedPremiumStatusSampled || smp.InvalidMs != 0 {
			break
		}
		if i+1 < len(s.Samples) && s.Samples[i+1].T != smp.T+feedPremiumSampleMs {
			break
		}
		if out.SampledRun == 0 {
			session = smp.Quote.Session
		}
		if smp.Quote.Session != session || smp.Context.Session != session {
			break
		}
		out.SampledRun++
	}
	if out.SampledRun > 0 {
		out.CoveredMs = int64(out.SampledRun-1) * feedPremiumSampleMs
	}
	if n := len(s.Samples); n > 0 {
		out.LastSampleMs = s.Samples[n-1].T
	}
	if g := s.openGap(); g != nil {
		out.OpenGap = true
		out.Status = feedObservationStatusGap
		out.Detail = fmt.Sprintf("%s since %d", g.Reason, g.StartMs)
		return out
	}
	if len(s.Samples) == 0 {
		out.Status = feedObservationStatusEmpty
		out.Detail = "no finalized premium samples since this owner started"
		return out
	}
	if age := nowMs - out.LastSampleMs; age > feedPremiumStaleAfterMs {
		out.Status = feedObservationStatusStale
		out.Detail = fmt.Sprintf("newest premium sample is %dms old, over %dms", age, feedPremiumStaleAfterMs)
		return out
	}
	if out.CoveredMs < s.WindowMs {
		out.Status = feedObservationStatusWarming
		out.Detail = fmt.Sprintf("%dms of contiguous sampled quote/oracle records in one session, %dms required", out.CoveredMs, s.WindowMs)
		return out
	}
	out.Status = feedObservationStatusReady
	out.Ready = true
	return out
}

func (o *marketFeedOwner) PremiumReadiness() []feedPremiumReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	out := make([]feedPremiumReadiness, 0, len(o.premiums))
	for _, k := range sortedPremiumKeys(o.premiums) {
		out = append(out, premiumReadiness(o.premiums[k], nowMs))
	}
	return out
}

type marketSnapshotPremium struct {
	Key       feedObservationKey
	WindowMs  int64
	CutoffMs  int64
	Samples   []feedPremiumSample
	Gaps      []feedObservationGap
	Readiness feedPremiumReadiness
}

func freezePremium(st *feedPremiumState, windowMs, cutoffMs, nowMs int64) *marketSnapshotPremium {
	start := cutoffMs - windowMs
	out := &marketSnapshotPremium{
		Key: st.Key, WindowMs: windowMs, CutoffMs: cutoffMs,
		Samples: []feedPremiumSample{}, Gaps: []feedObservationGap{},
		Readiness: premiumReadiness(st, nowMs),
	}
	for _, smp := range st.Samples {
		if smp.T < start || smp.T > cutoffMs {
			continue
		}
		cp := *smp
		if cp.Quote != nil {
			q := *cp.Quote
			cp.Quote = &q
		}
		if cp.Context != nil {
			c := *cp.Context
			cp.Context = &c
		}
		if cp.InvalidMs > cutoffMs {
			cp.InvalidMs, cp.InvalidWhy = 0, ""
		}
		out.Samples = append(out.Samples, cp)
	}
	for _, g := range st.Gaps {
		end := g.EndMs
		if end != 0 && end < start {
			continue
		}
		if g.StartMs > cutoffMs || g.DetectedMs > cutoffMs {
			continue
		}
		if end > cutoffMs {
			end = 0
		}
		out.Gaps = append(out.Gaps, feedObservationGap{StartMs: g.StartMs, EndMs: end, DetectedMs: g.DetectedMs, Reason: g.Reason})
	}
	return out
}

type marketPremiumQuote struct {
	Bid     string `json:"bid"`
	BidSz   string `json:"bid_sz"`
	BidN    int64  `json:"bid_n"`
	Ask     string `json:"ask"`
	AskSz   string `json:"ask_sz"`
	AskN    int64  `json:"ask_n"`
	EventMs int64  `json:"event_ms"`
	RecvMs  int64  `json:"recv_ms"`
	Session uint64 `json:"session"`
	Seq     uint64 `json:"seq"`
}

type marketPremiumContext struct {
	Oracle        string  `json:"oracle"`
	Mark          string  `json:"mark"`
	Mid           *string `json:"mid"`
	OracleEventMs *int64  `json:"oracle_event_ms"`
	RecvMs        int64   `json:"recv_ms"`
	Session       uint64  `json:"session"`
	Seq           uint64  `json:"seq"`
}

type marketPremiumSample struct {
	T           int64                 `json:"t"`
	Status      string                `json:"status"`
	Reason      string                `json:"reason,omitempty"`
	Quote       *marketPremiumQuote   `json:"quote"`
	Context     *marketPremiumContext `json:"context"`
	FinalizedMs int64                 `json:"finalized_ms"`
	InvalidMs   *int64                `json:"invalid_ms"`
	InvalidWhy  string                `json:"invalid_reason,omitempty"`
}

type marketPremiumPayload struct {
	Kind            string                 `json:"kind"`
	Coin            string                 `json:"coin"`
	Source          string                 `json:"source"`
	Contract        string                 `json:"contract"`
	TimeBasis       string                 `json:"time_basis"`
	OracleTimeBasis string                 `json:"oracle_time_basis"`
	SampleMs        int64                  `json:"sample_ms"`
	MaxFutureSkewMs int64                  `json:"max_future_skew_ms"`
	WindowMs        int64                  `json:"window_ms"`
	CutoffMs        int64                  `json:"cutoff_ms"`
	BarIntervalMs   int64                  `json:"bar_interval_ms"`
	SnapshotID      string                 `json:"snapshot_id"`
	Available       bool                   `json:"available"`
	Reason          string                 `json:"reason,omitempty"`
	FeedStatus      string                 `json:"feed_status,omitempty"`
	Samples         []marketPremiumSample  `json:"samples"`
	Gaps            []marketObservationGap `json:"gaps"`
}

func newMarketPremiumPayload(coin string, windowMs, barIntervalMs int64) marketPremiumPayload {
	return marketPremiumPayload{
		Kind: feedObservationPerpPremium, Coin: coin, Source: feedPremiumSourceHLWS, Contract: feedPremiumContract,
		TimeBasis: feedPremiumTimeBasis, OracleTimeBasis: feedPremiumOracleTimeBasis, SampleMs: feedPremiumSampleMs,
		MaxFutureSkewMs: feedPremiumMaxFutureSkewMs, WindowMs: windowMs, BarIntervalMs: barIntervalMs,
		Samples: []marketPremiumSample{}, Gaps: []marketObservationGap{},
	}
}

func premiumPayloadSample(smp feedPremiumSample) marketPremiumSample {
	out := marketPremiumSample{T: smp.T, Status: smp.Status, Reason: smp.Reason, FinalizedMs: smp.FinalizedMs}
	if q := smp.Quote; q != nil {
		out.Quote = &marketPremiumQuote{Bid: q.BidPx, BidSz: q.BidSz, BidN: q.BidN, Ask: q.AskPx, AskSz: q.AskSz, AskN: q.AskN,
			EventMs: q.EventMs, RecvMs: q.RecvMs, Session: q.Session, Seq: q.Seq}
	}
	if c := smp.Context; c != nil {
		mc := &marketPremiumContext{Oracle: c.OraclePx, Mark: c.MarkPx, RecvMs: c.RecvMs, Session: c.Session, Seq: c.Seq}
		if c.MidPx != "" {
			mid := c.MidPx
			mc.Mid = &mid
		}
		out.Context = mc
	}
	if smp.InvalidMs != 0 {
		v := smp.InvalidMs
		out.InvalidMs = &v
		out.InvalidWhy = smp.InvalidWhy
	}
	return out
}

func (s *marketSnapshot) premiumPayload(key feedObservationKey, windowMs, barIntervalMs int64) marketPremiumPayload {
	out := newMarketPremiumPayload(key.Coin, windowMs, barIntervalMs)
	if s == nil {
		out.Reason = "no sealed market snapshot"
		return out
	}
	out.SnapshotID = s.EvaluationID
	entry, ok := s.premiums[key]
	if !ok || entry == nil {
		out.Reason = fmt.Sprintf("%s is not in the sealed snapshot", key.PayloadID())
		out.CutoffMs = s.observationCutoff().UnixMilli()
		return out
	}
	out.CutoffMs = entry.CutoffMs
	out.FeedStatus = entry.Readiness.Status
	if entry.WindowMs < windowMs {
		out.Reason = fmt.Sprintf("%s window %dms in the seal is below the %dms this strategy needs", key.PayloadID(), entry.WindowMs, windowMs)
		return out
	}
	start := entry.CutoffMs - windowMs
	for _, smp := range entry.Samples {
		if smp.T < start || smp.T > entry.CutoffMs {
			continue
		}
		out.Samples = append(out.Samples, premiumPayloadSample(smp))
	}
	for _, g := range entry.Gaps {
		if g.EndMs != 0 && g.EndMs < start {
			continue
		}
		mg := marketObservationGap{StartMs: g.StartMs, DetectedMs: g.DetectedMs, Reason: g.Reason}
		if g.EndMs != 0 {
			end := g.EndMs
			mg.EndMs = &end
		}
		out.Gaps = append(out.Gaps, mg)
	}
	out.Available = true
	return out
}

func attachPremiumPayloads(payload *marketPayload, s *marketSnapshot, needs map[feedObservationKey]int64, barIntervalMs int64) {
	if payload == nil {
		return
	}
	var keys []feedObservationKey
	for k := range needs {
		if k.Kind == feedObservationPerpPremium {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sortFeedObservationKeys(keys)
	payload.Premiums = make(map[string]marketPremiumPayload, len(keys))
	for _, k := range keys {
		payload.Premiums[k.PayloadID()] = s.premiumPayload(k, needs[k], barIntervalMs)
	}
	payload.Version = marketPayloadVersionPremium
}

func perpetualPremiumWindowMs(sc StrategyConfig, intervalMs int64) (int64, error) {
	lookback, err := openInterestParamInt(sc.OpenStrategy.Params, "lookback_samples", perpetualPremiumDefaultLookback)
	if err != nil {
		return 0, err
	}
	if lookback > perpetualPremiumMaxLookback {
		return 0, fmt.Errorf("lookback_samples %d is over %d", lookback, perpetualPremiumMaxLookback)
	}
	if intervalMs <= 0 {
		intervalMs = perpetualPremiumDefaultIntervalMs
	}
	window := (lookback+1)*feedPremiumSampleMs + intervalMs
	if window > feedPremiumMaxWindowMs {
		return 0, fmt.Errorf("premium window %dms (lookback_samples %d) is over the %dms retention cap", window, lookback, feedPremiumMaxWindowMs)
	}
	return window, nil
}

func feedStrategyUsesPremium(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == perpetualPremiumStrategyName
}

type feedSealPremiumQuote struct {
	Bid     string `json:"b"`
	BidSz   string `json:"bs"`
	BidN    int64  `json:"bn"`
	Ask     string `json:"a"`
	AskSz   string `json:"as"`
	AskN    int64  `json:"an"`
	EventMs int64  `json:"e"`
	RecvMs  int64  `json:"r"`
	Session uint64 `json:"s"`
	Seq     uint64 `json:"q"`
}

type feedSealPremiumContext struct {
	Oracle  string `json:"o"`
	Mark    string `json:"m"`
	Mid     string `json:"d"`
	RecvMs  int64  `json:"r"`
	Session uint64 `json:"s"`
	Seq     uint64 `json:"q"`
}

type feedSealPremiumSample struct {
	T           int64                   `json:"t"`
	Status      string                  `json:"st"`
	Reason      string                  `json:"why"`
	Quote       *feedSealPremiumQuote   `json:"qt"`
	Context     *feedSealPremiumContext `json:"cx"`
	FinalizedMs int64                   `json:"f"`
	InvalidMs   int64                   `json:"x"`
	InvalidWhy  string                  `json:"xw"`
}

type feedSealPremiumReadiness struct {
	Status          string `json:"status"`
	Detail          string `json:"detail"`
	Ready           bool   `json:"ready"`
	Samples         int    `json:"samples"`
	SampledRun      int    `json:"sampled_run"`
	CoveredMs       int64  `json:"covered_ms"`
	LastSampleMs    int64  `json:"last_sample_ms"`
	LastRecvMs      int64  `json:"last_recv_ms"`
	OpenGap         bool   `json:"open_gap"`
	QuoteFrames     uint64 `json:"quote_frames"`
	QuoteAccepted   uint64 `json:"quote_accepted"`
	QuoteOneSided   uint64 `json:"quote_one_sided"`
	QuoteDuplicates uint64 `json:"quote_duplicates"`
	QuoteConflicts  uint64 `json:"quote_conflicts"`
	QuoteOutOfOrder uint64 `json:"quote_out_of_order"`
	CtxFrames       uint64 `json:"context_frames"`
	CtxAccepted     uint64 `json:"context_accepted"`
	Rejected        uint64 `json:"rejected"`
	Finalized       uint64 `json:"finalized"`
	Sampled         uint64 `json:"sampled"`
	Missing         uint64 `json:"missing"`
	Invalid         uint64 `json:"invalid"`
	LastReject      string `json:"last_reject"`
}

type feedSealPremium struct {
	Host            string                   `json:"host"`
	Namespace       string                   `json:"namespace"`
	Coin            string                   `json:"coin"`
	Kind            string                   `json:"kind"`
	Source          string                   `json:"source"`
	Contract        string                   `json:"contract"`
	TimeBasis       string                   `json:"time_basis"`
	OracleTimeBasis string                   `json:"oracle_time_basis"`
	SampleMs        int64                    `json:"sample_ms"`
	MaxFutureSkewMs int64                    `json:"max_future_skew_ms"`
	WindowMs        int64                    `json:"window_ms"`
	CutoffMs        int64                    `json:"cutoff_ms"`
	Readiness       feedSealPremiumReadiness `json:"readiness"`
	Samples         []feedSealPremiumSample  `json:"samples"`
	Gaps            []feedSealObservationGap `json:"gaps"`
}

func sealPremiums(m map[feedObservationKey]*marketSnapshotPremium) []feedSealPremium {
	keys := make([]feedObservationKey, 0, len(m))
	for k, v := range m {
		if v != nil {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sortFeedObservationKeys(keys)
	out := make([]feedSealPremium, 0, len(keys))
	for _, k := range keys {
		p := m[k]
		r := p.Readiness
		sp := feedSealPremium{
			Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, Kind: k.Kind,
			Source: feedPremiumSourceHLWS, Contract: feedPremiumContract, TimeBasis: feedPremiumTimeBasis,
			OracleTimeBasis: feedPremiumOracleTimeBasis, SampleMs: feedPremiumSampleMs, MaxFutureSkewMs: feedPremiumMaxFutureSkewMs,
			WindowMs: p.WindowMs, CutoffMs: p.CutoffMs,
			Readiness: feedSealPremiumReadiness{
				Status: r.Status, Detail: r.Detail, Ready: r.Ready, Samples: r.Samples, SampledRun: r.SampledRun,
				CoveredMs: r.CoveredMs, LastSampleMs: r.LastSampleMs, LastRecvMs: r.LastRecvMs, OpenGap: r.OpenGap,
				QuoteFrames: r.Stats.QuoteFrames, QuoteAccepted: r.Stats.QuoteAccepted, QuoteOneSided: r.Stats.QuoteOneSided,
				QuoteDuplicates: r.Stats.QuoteDuplicates, QuoteConflicts: r.Stats.QuoteConflicts, QuoteOutOfOrder: r.Stats.QuoteOutOfOrder,
				CtxFrames: r.Stats.CtxFrames, CtxAccepted: r.Stats.CtxAccepted, Rejected: r.Stats.Rejected, Finalized: r.Stats.Finalized,
				Sampled: r.Stats.Sampled, Missing: r.Stats.Missing, Invalid: r.Stats.Invalid, LastReject: r.Stats.LastReject,
			},
			Samples: make([]feedSealPremiumSample, 0, len(p.Samples)),
			Gaps:    make([]feedSealObservationGap, 0, len(p.Gaps)),
		}
		for _, smp := range p.Samples {
			ss := feedSealPremiumSample{T: smp.T, Status: smp.Status, Reason: smp.Reason, FinalizedMs: smp.FinalizedMs, InvalidMs: smp.InvalidMs, InvalidWhy: smp.InvalidWhy}
			if q := smp.Quote; q != nil {
				ss.Quote = &feedSealPremiumQuote{Bid: q.BidPx, BidSz: q.BidSz, BidN: q.BidN, Ask: q.AskPx, AskSz: q.AskSz, AskN: q.AskN,
					EventMs: q.EventMs, RecvMs: q.RecvMs, Session: q.Session, Seq: q.Seq}
			}
			if c := smp.Context; c != nil {
				ss.Context = &feedSealPremiumContext{Oracle: c.OraclePx, Mark: c.MarkPx, Mid: c.MidPx, RecvMs: c.RecvMs, Session: c.Session, Seq: c.Seq}
			}
			sp.Samples = append(sp.Samples, ss)
		}
		for _, g := range p.Gaps {
			sp.Gaps = append(sp.Gaps, feedSealObservationGap{StartMs: g.StartMs, EndMs: g.EndMs, DetectedMs: g.DetectedMs, Reason: g.Reason})
		}
		out = append(out, sp)
	}
	return out
}

func sealPremiumDecimal(text string) (float64, error) {
	c, err := canonicalFeedDecimal(text)
	if err != nil {
		return 0, err
	}
	if c != text {
		return 0, fmt.Errorf("%q is not canonical (%q)", text, c)
	}
	v, err := strconv.ParseFloat(c, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("%q is not a positive finite decimal", text)
	}
	return v, nil
}

func validateSealPremiumQuote(q *feedSealPremiumQuote, t int64) error {
	bid, err := sealPremiumDecimal(q.Bid)
	if err != nil {
		return fmt.Errorf("bid: %w", err)
	}
	ask, err := sealPremiumDecimal(q.Ask)
	if err != nil {
		return fmt.Errorf("ask: %w", err)
	}
	if _, err := sealPremiumDecimal(q.BidSz); err != nil {
		return fmt.Errorf("bid size: %w", err)
	}
	if _, err := sealPremiumDecimal(q.AskSz); err != nil {
		return fmt.Errorf("ask size: %w", err)
	}
	switch {
	case bid >= ask:
		return fmt.Errorf("bid %s is not below ask %s", q.Bid, q.Ask)
	case q.BidN < 0 || q.AskN < 0:
		return errors.New("negative order count")
	case q.EventMs <= 0 || q.RecvMs <= 0 || q.RecvMs > t:
		return fmt.Errorf("quote event %d / receipt %d is not a positive receipt at or before %d", q.EventMs, q.RecvMs, t)
	case q.EventMs > q.RecvMs+feedPremiumMaxFutureSkewMs:
		return fmt.Errorf("quote event %d is over %dms after its receipt %d", q.EventMs, feedPremiumMaxFutureSkewMs, q.RecvMs)
	case q.Seq == 0 || q.Session == 0:
		return errors.New("quote has no session or sequence")
	}
	return nil
}

func validateSealPremiumContext(c *feedSealPremiumContext, t int64) error {
	if _, err := sealPremiumDecimal(c.Oracle); err != nil {
		return fmt.Errorf("oracle: %w", err)
	}
	if _, err := sealPremiumDecimal(c.Mark); err != nil {
		return fmt.Errorf("mark: %w", err)
	}
	if c.Mid != "" {
		if _, err := sealPremiumDecimal(c.Mid); err != nil {
			return fmt.Errorf("mid: %w", err)
		}
	}
	switch {
	case c.RecvMs <= 0 || c.RecvMs > t:
		return fmt.Errorf("context receipt %d is not a positive receipt at or before %d", c.RecvMs, t)
	case c.Seq == 0 || c.Session == 0:
		return errors.New("context has no session or sequence")
	}
	return nil
}

func validateFeedSealPremiums(premiums []feedSealPremium) error {
	var prev *feedObservationKey
	for i := range premiums {
		p := premiums[i]
		key := feedObservationKey{Host: p.Host, Namespace: p.Namespace, Coin: p.Coin, Kind: p.Kind}
		if prev != nil && !feedObservationKeyLess(*prev, key) {
			return fmt.Errorf("seal premium records are not strictly ordered at %s", key)
		}
		prev = &key
		if p.Kind != feedObservationPerpPremium {
			return fmt.Errorf("seal premium entry %s has kind %q", key, p.Kind)
		}
		if p.Source != feedPremiumSourceHLWS || p.Contract != feedPremiumContract || p.TimeBasis != feedPremiumTimeBasis ||
			p.OracleTimeBasis != feedPremiumOracleTimeBasis || p.SampleMs != feedPremiumSampleMs || p.MaxFutureSkewMs != feedPremiumMaxFutureSkewMs {
			return fmt.Errorf("seal premium entry %s declares source %q contract %q time basis %q oracle time basis %q sample %d skew %d; this consumer accepts only %q %q %q %q %d %d",
				key, p.Source, p.Contract, p.TimeBasis, p.OracleTimeBasis, p.SampleMs, p.MaxFutureSkewMs,
				feedPremiumSourceHLWS, feedPremiumContract, feedPremiumTimeBasis, feedPremiumOracleTimeBasis, feedPremiumSampleMs, feedPremiumMaxFutureSkewMs)
		}
		if p.WindowMs <= 0 || p.CutoffMs <= 0 || p.WindowMs > feedPremiumMaxWindowMs {
			return fmt.Errorf("seal premium entry %s has window %d, cutoff %d", key, p.WindowMs, p.CutoffMs)
		}
		start := p.CutoffMs - p.WindowMs
		var lastT int64
		for j, smp := range p.Samples {
			if smp.T%feedPremiumSampleMs != 0 || smp.T < start || smp.T > p.CutoffMs {
				return fmt.Errorf("seal premium entry %s sample %d at %d is outside [%d, %d] or off the %dms grid", key, j, smp.T, start, p.CutoffMs, feedPremiumSampleMs)
			}
			if j > 0 && smp.T <= lastT {
				return fmt.Errorf("seal premium entry %s samples are not strictly ordered at sample %d", key, j)
			}
			lastT = smp.T
			if smp.FinalizedMs <= smp.T {
				return fmt.Errorf("seal premium entry %s sample %d finalized at %d, not after its grid time %d", key, j, smp.FinalizedMs, smp.T)
			}
			switch smp.Status {
			case feedPremiumStatusSampled:
				if smp.Reason != "" || smp.Quote == nil || smp.Context == nil {
					return fmt.Errorf("seal premium entry %s sampled record %d lacks a quote or context or carries a reason", key, j)
				}
			case feedPremiumStatusMissing, feedPremiumStatusInvalid:
				if smp.Reason == "" {
					return fmt.Errorf("seal premium entry %s %s record %d has no reason", key, smp.Status, j)
				}
			default:
				return fmt.Errorf("seal premium entry %s record %d has status %q", key, j, smp.Status)
			}
			if smp.Quote != nil {
				if err := validateSealPremiumQuote(smp.Quote, smp.T); err != nil {
					return fmt.Errorf("seal premium entry %s record %d quote: %w", key, j, err)
				}
			}
			if smp.Context != nil {
				if err := validateSealPremiumContext(smp.Context, smp.T); err != nil {
					return fmt.Errorf("seal premium entry %s record %d context: %w", key, j, err)
				}
			}
			if (smp.InvalidMs == 0) != (smp.InvalidWhy == "") || (smp.InvalidMs != 0 && (smp.InvalidMs <= smp.T || smp.InvalidMs > p.CutoffMs)) {
				return fmt.Errorf("seal premium entry %s record %d has a malformed later invalidation", key, j)
			}
		}
		for j, g := range p.Gaps {
			if g.StartMs <= 0 || (g.EndMs != 0 && g.EndMs < g.StartMs) || g.Reason == "" || g.DetectedMs < g.StartMs || g.DetectedMs > p.CutoffMs {
				return fmt.Errorf("seal premium entry %s gap %d is malformed", key, j)
			}
		}
	}
	return nil
}

func snapshotPremiums(premiums []feedSealPremium) map[feedObservationKey]*marketSnapshotPremium {
	out := make(map[feedObservationKey]*marketSnapshotPremium, len(premiums))
	for _, p := range premiums {
		key := feedObservationKey{Host: p.Host, Namespace: p.Namespace, Coin: p.Coin, Kind: p.Kind}
		r := p.Readiness
		entry := &marketSnapshotPremium{
			Key: key, WindowMs: p.WindowMs, CutoffMs: p.CutoffMs,
			Samples: make([]feedPremiumSample, 0, len(p.Samples)),
			Gaps:    make([]feedObservationGap, 0, len(p.Gaps)),
			Readiness: feedPremiumReadiness{
				Key: key, Status: r.Status, Detail: r.Detail, Ready: r.Ready, Samples: r.Samples, SampledRun: r.SampledRun,
				CoveredMs: r.CoveredMs, WindowMs: p.WindowMs, LastSampleMs: r.LastSampleMs, LastRecvMs: r.LastRecvMs, OpenGap: r.OpenGap,
				Stats: feedPremiumStats{QuoteFrames: r.QuoteFrames, QuoteAccepted: r.QuoteAccepted, QuoteOneSided: r.QuoteOneSided,
					QuoteDuplicates: r.QuoteDuplicates, QuoteConflicts: r.QuoteConflicts, QuoteOutOfOrder: r.QuoteOutOfOrder,
					CtxFrames: r.CtxFrames, CtxAccepted: r.CtxAccepted, Rejected: r.Rejected, Finalized: r.Finalized,
					Sampled: r.Sampled, Missing: r.Missing, Invalid: r.Invalid, LastReject: r.LastReject},
			},
		}
		for _, smp := range p.Samples {
			fs := feedPremiumSample{T: smp.T, Status: smp.Status, Reason: smp.Reason, FinalizedMs: smp.FinalizedMs, InvalidMs: smp.InvalidMs, InvalidWhy: smp.InvalidWhy}
			if q := smp.Quote; q != nil {
				fs.Quote = &feedPremiumQuote{BidPx: q.Bid, BidSz: q.BidSz, BidN: q.BidN, AskPx: q.Ask, AskSz: q.AskSz, AskN: q.AskN,
					EventMs: q.EventMs, RecvMs: q.RecvMs, Session: q.Session, Seq: q.Seq}
			}
			if c := smp.Context; c != nil {
				fs.Context = &feedPremiumContext{OraclePx: c.Oracle, MarkPx: c.Mark, MidPx: c.Mid, RecvMs: c.RecvMs, Session: c.Session, Seq: c.Seq}
			}
			entry.Samples = append(entry.Samples, fs)
		}
		for _, g := range p.Gaps {
			entry.Gaps = append(entry.Gaps, feedObservationGap{StartMs: g.StartMs, EndMs: g.EndMs, DetectedMs: g.DetectedMs, Reason: g.Reason})
		}
		out[key] = entry
	}
	return out
}
