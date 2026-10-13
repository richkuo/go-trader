package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	feedObservationTradeFlow = "trade_flow"
	feedTradeSourceHLWS      = "hyperliquid_ws_trades"
	feedTradeUnitsBase       = "base"
	feedTradeTimeBasis       = "event_time_receipt_cutoff"
	feedTradeIdentity        = "venue+namespace+coin+tid"
	feedTradeSideEvidence    = "hyperliquid-trades-side-aggressor/1638-v1"
	feedTradeIntervalMs      = int64(60_000)
	feedTradeReorderGraceMs  = int64(5_000)
	feedTradeMaxEventGapMs   = int64(30_000)
	feedTradeMaxFutureSkewMs = int64(5_000)
	feedTradeIdentityRetain  = int64(15 * 60_000)
	feedTradeIdentityCap     = 400_000
	feedTradeDecimalScale    = 8
	feedTradeMaxWindowMs     = int64(26 * 3_600_000)
	feedTradeRetainSlackMs   = int64(10 * 60_000)
	feedTradeMaxIntervals    = int((feedTradeMaxWindowMs + feedTradeRetainSlackMs) / feedTradeIntervalMs)

	feedTradeStatusOpen     = "open"
	feedTradeStatusComplete = "complete"
	feedTradeStatusInvalid  = "invalid"

	feedTradeOutcomeAccepted  = "accepted"
	feedTradeOutcomeDuplicate = "duplicate"
	feedTradeOutcomeConflict  = "conflict"
	feedTradeOutcomeLate      = "late"
	feedTradeOutcomeUncovered = "uncovered"
	feedTradeOutcomeRejected  = "rejected"

	feedTradeGapDisconnected = "disconnected"
	feedTradeGapSilence      = "silence"
	feedTradeGapMalformed    = "malformed"
	feedTradeGapClock        = "clock_regression"
	feedTradeGapIdentity     = "identity_overflow"

	feedTradeStatusNoCoverage = "no_coverage"

	cumulativeVolumeDeltaStrategyName = "cumulative_volume_delta_divergence"
	cvdDefaultSessionHours            = 4
)

var feedTradeScale = int64(math.Pow10(feedTradeDecimalScale))

func tradeFlowKeyFor(coin string) feedObservationKey {
	return feedObservationKey{Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, Kind: feedObservationTradeFlow}
}

func feedObservationSourceFor(kind string) string {
	if kind == feedObservationTradeFlow {
		return feedTradeSourceHLWS
	}
	return feedObservationSourceHLWS
}

type feedTrade struct {
	Coin   string
	Tid    uint64
	TimeMs int64
	Side   string
	Px     string
	Sz     string
	Units  int64
	Hash   string
	Buyer  string
	Seller string
}

func (t feedTrade) canonical() string {
	return strings.Join([]string{t.Coin, strconv.FormatUint(t.Tid, 10), strconv.FormatInt(t.TimeMs, 10), t.Side, t.Px, t.Sz, t.Hash, t.Buyer, t.Seller}, "|")
}

func (t feedTrade) digest() [32]byte {
	return sha256.Sum256([]byte(t.canonical()))
}

func canonicalFeedDecimal(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty decimal")
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && frac == "") {
		return "", fmt.Errorf("decimal %q is malformed", raw)
	}
	for _, r := range intPart + frac {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("decimal %q is not a plain nonnegative decimal", raw)
		}
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return intPart, nil
	}
	return intPart + "." + frac, nil
}

func feedDecimalUnits(canonical string) (int64, error) {
	intPart, frac, _ := strings.Cut(canonical, ".")
	if len(frac) > feedTradeDecimalScale {
		return 0, fmt.Errorf("size %s has more than %d decimal places", canonical, feedTradeDecimalScale)
	}
	if len(intPart) > 10 {
		return 0, fmt.Errorf("size %s is too large for the trade-volume accumulator", canonical)
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, err
	}
	frac += strings.Repeat("0", feedTradeDecimalScale-len(frac))
	part, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	return whole*feedTradeScale + part, nil
}

func formatFeedUnits(units int64) string {
	if units < 0 {
		return "-" + formatFeedUnits(-units)
	}
	whole := units / feedTradeScale
	part := units % feedTradeScale
	if part == 0 {
		return strconv.FormatInt(whole, 10)
	}
	frac := fmt.Sprintf("%0*d", feedTradeDecimalScale, part)
	return strconv.FormatInt(whole, 10) + "." + strings.TrimRight(frac, "0")
}

func addFeedUnits(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func addFeedTradeHash(acc *[32]byte, d [32]byte) {
	carry := 0
	for i := 31; i >= 0; i-- {
		sum := int(acc[i]) + int(d[i]) + carry
		acc[i] = byte(sum & 0xff)
		carry = sum >> 8
	}
}

type feedSocketTrade struct {
	Coin  string          `json:"coin"`
	Side  string          `json:"side"`
	Px    string          `json:"px"`
	Sz    string          `json:"sz"`
	Time  json.Number     `json:"time"`
	Hash  string          `json:"hash"`
	Tid   json.Number     `json:"tid"`
	Users json.RawMessage `json:"users"`
}

type feedTradeParse struct {
	Trade feedTrade
	Err   error
	Tid   uint64
}

func parseFeedTradeElement(raw json.RawMessage) feedTradeParse {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var msg feedSocketTrade
	if err := dec.Decode(&msg); err != nil {
		return feedTradeParse{Err: fmt.Errorf("decode trade: %w", err)}
	}
	tid, tidErr := strconv.ParseUint(msg.Tid.String(), 10, 64)
	out := feedTradeParse{Tid: tid}
	coin := strings.TrimSpace(msg.Coin)
	if coin == "" {
		out.Err = errors.New("trade carries no coin")
		return out
	}
	out.Trade.Coin = coin
	if tidErr != nil {
		out.Err = fmt.Errorf("trade tid %q is not an unsigned integer", msg.Tid.String())
		return out
	}
	timeMs, err := strconv.ParseInt(msg.Time.String(), 10, 64)
	if err != nil || timeMs <= 0 {
		out.Err = fmt.Errorf("trade time %q is not a positive millisecond timestamp", msg.Time.String())
		return out
	}
	if msg.Side != "B" && msg.Side != "A" {
		out.Err = fmt.Errorf("trade side %q is not B or A", msg.Side)
		return out
	}
	px, err := canonicalFeedDecimal(msg.Px)
	if err != nil {
		out.Err = fmt.Errorf("trade px: %w", err)
		return out
	}
	if pxv, perr := strconv.ParseFloat(px, 64); perr != nil || !isFiniteFeedPrice(pxv) {
		out.Err = fmt.Errorf("trade px %q is not a positive finite price", msg.Px)
		return out
	}
	sz, err := canonicalFeedDecimal(msg.Sz)
	if err != nil {
		out.Err = fmt.Errorf("trade sz: %w", err)
		return out
	}
	units, err := feedDecimalUnits(sz)
	if err != nil {
		out.Err = fmt.Errorf("trade sz: %w", err)
		return out
	}
	if units <= 0 {
		out.Err = fmt.Errorf("trade sz %q is not positive", msg.Sz)
		return out
	}
	var users []string
	if err := json.Unmarshal(msg.Users, &users); err != nil || len(users) != 2 {
		out.Err = errors.New("trade users is not a [buyer, seller] pair")
		return out
	}
	out.Trade = feedTrade{
		Coin: coin, Tid: tid, TimeMs: timeMs, Side: msg.Side, Px: px, Sz: sz, Units: units,
		Hash: strings.ToLower(strings.TrimSpace(msg.Hash)), Buyer: strings.ToLower(strings.TrimSpace(users[0])), Seller: strings.ToLower(strings.TrimSpace(users[1])),
	}
	return out
}

func parseFeedTradesFrame(data json.RawMessage) ([]json.RawMessage, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(data, &elems); err != nil {
		return nil, fmt.Errorf("decode trades frame: %w", err)
	}
	return elems, nil
}

type feedSubscriptionAck struct {
	Method       string `json:"method"`
	Subscription struct {
		Type string `json:"type"`
		Coin string `json:"coin"`
	} `json:"subscription"`
}

type feedTradeInterval struct {
	StartMs     int64
	BuyUnits    int64
	SellUnits   int64
	Trades      int
	Hash        [32]byte
	FirstRecvMs int64
	LastRecvMs  int64
	Session     uint64
	Status      string
	Reason      string
	FinalizedMs int64
	InvalidMs   int64
}

func (iv *feedTradeInterval) invalidate(atMs int64, reason string) bool {
	if iv.InvalidMs != 0 {
		return false
	}
	iv.InvalidMs = atMs
	iv.Reason = reason
	return true
}

type feedTradeIdent struct {
	Digest  [32]byte
	StartMs int64
	RecvMs  int64
}

type feedTradeFlowStats struct {
	Frames     uint64
	Received   uint64
	Accepted   uint64
	Duplicates uint64
	Conflicts  uint64
	Late       uint64
	Uncovered  uint64
	Rejected   uint64
	Finalized  uint64
	Invalid    uint64
	LastReject string
}

type feedTradeFlowState struct {
	Key          feedObservationKey
	WindowMs     int64
	Intervals    []*feedTradeInterval
	Gaps         []feedObservationGap
	CoverFromMs  int64
	FrontierMs   int64
	Session      uint64
	LastFrameMs  int64
	LastRecvMs   int64
	IdentFloorMs int64
	Resumable    bool
	Stats        feedTradeFlowStats

	idents     map[uint64]feedTradeIdent
	identOrder []uint64
}

func newFeedTradeFlowState(key feedObservationKey, windowMs int64) *feedTradeFlowState {
	return &feedTradeFlowState{Key: key, WindowMs: windowMs, idents: make(map[uint64]feedTradeIdent)}
}

func (s *feedTradeFlowState) raiseWindow(windowMs int64) {
	if windowMs > s.WindowMs {
		s.WindowMs = windowMs
	}
}

func feedTradeIntervalStart(ms int64) int64 {
	return ms - ((ms%feedTradeIntervalMs)+feedTradeIntervalMs)%feedTradeIntervalMs
}

func (s *feedTradeFlowState) interval(startMs int64) *feedTradeInterval {
	i := sort.Search(len(s.Intervals), func(i int) bool { return s.Intervals[i].StartMs >= startMs })
	if i < len(s.Intervals) && s.Intervals[i].StartMs == startMs {
		return s.Intervals[i]
	}
	return nil
}

func (s *feedTradeFlowState) ensureInterval(startMs int64) *feedTradeInterval {
	i := sort.Search(len(s.Intervals), func(i int) bool { return s.Intervals[i].StartMs >= startMs })
	if i < len(s.Intervals) && s.Intervals[i].StartMs == startMs {
		return s.Intervals[i]
	}
	iv := &feedTradeInterval{StartMs: startMs, Status: feedTradeStatusOpen, Session: s.Session}
	s.Intervals = append(s.Intervals, nil)
	copy(s.Intervals[i+1:], s.Intervals[i:])
	s.Intervals[i] = iv
	return iv
}

func (s *feedTradeFlowState) openGap() *feedObservationGap {
	if n := len(s.Gaps); n > 0 && s.Gaps[n-1].EndMs == 0 {
		return &s.Gaps[n-1]
	}
	return nil
}

type feedTradeEvent struct {
	Kind   string
	Fields map[string]any
}

type feedTradeEvents []feedTradeEvent

func (e *feedTradeEvents) add(kind string, fields map[string]any) {
	*e = append(*e, feedTradeEvent{Kind: kind, Fields: fields})
}

func intervalEventFields(s *feedTradeFlowState, iv *feedTradeInterval) map[string]any {
	return map[string]any{
		"coin": s.Key.Coin, "kind": feedObservationTradeFlow, "start_ms": iv.StartMs, "status": iv.Status,
		"buy": formatFeedUnits(iv.BuyUnits), "sell": formatFeedUnits(iv.SellUnits), "trades": iv.Trades,
		"hash": hex.EncodeToString(iv.Hash[:]), "session": iv.Session, "finalized_ms": iv.FinalizedMs,
		"invalid_ms": iv.InvalidMs, "reason": iv.Reason, "recv_ms": iv.FinalizedMs,
	}
}

func (s *feedTradeFlowState) stopCoverage(atMs int64, reason string, ev *feedTradeEvents) {
	if s.CoverFromMs == 0 {
		s.Resumable = false
		return
	}
	s.Resumable = false
	for _, iv := range s.Intervals {
		if iv.StartMs < s.FrontierMs || iv.Status != feedTradeStatusOpen {
			continue
		}
		iv.invalidate(atMs, reason)
	}
	for start := s.FrontierMs; start < atMs && start-s.FrontierMs < feedTradeMaxWindowMs; start += feedTradeIntervalMs {
		iv := s.ensureInterval(start)
		iv.invalidate(atMs, reason)
	}
	for _, iv := range s.Intervals {
		if iv.Status != feedTradeStatusOpen {
			continue
		}
		iv.Status = feedTradeStatusInvalid
		iv.FinalizedMs = atMs
		s.Stats.Finalized++
		s.Stats.Invalid++
		ev.add("interval", intervalEventFields(s, iv))
	}
	start := s.LastFrameMs
	if start == 0 || start > atMs {
		start = atMs
	}
	if g := s.openGap(); g == nil {
		s.Gaps = append(s.Gaps, feedObservationGap{StartMs: start, DetectedMs: atMs, Reason: reason})
	}
	ev.add("cover", map[string]any{"coin": s.Key.Coin, "kind": feedObservationTradeFlow, "state": "stop", "reason": reason, "recv_ms": atMs, "frontier_ms": s.FrontierMs})
	s.CoverFromMs = 0
	s.FrontierMs = 0
}

func (s *feedTradeFlowState) startCoverage(recvMs int64, session uint64, ev *feedTradeEvents) {
	if s.CoverFromMs != 0 && s.Session == session {
		return
	}
	if s.CoverFromMs != 0 {
		s.stopCoverage(recvMs, feedTradeGapDisconnected, ev)
	}
	s.Session = session
	s.beginCoverage(recvMs, "subscribed", ev)
}

func (s *feedTradeFlowState) beginCoverage(recvMs int64, reason string, ev *feedTradeEvents) {
	s.Resumable = false
	s.CoverFromMs = feedTradeIntervalStart(recvMs) + feedTradeIntervalMs
	s.FrontierMs = s.CoverFromMs
	s.LastFrameMs = recvMs
	if g := s.openGap(); g != nil {
		g.EndMs = s.CoverFromMs
	}
	ev.add("cover", map[string]any{"coin": s.Key.Coin, "kind": feedObservationTradeFlow, "state": "start", "reason": reason, "from_ms": s.CoverFromMs, "recv_ms": recvMs, "session": s.Session})
}

func (s *feedTradeFlowState) advance(nowMs int64, ev *feedTradeEvents) {
	if s.CoverFromMs == 0 {
		return
	}
	if s.LastFrameMs > 0 && nowMs-s.LastFrameMs > feedTradeMaxEventGapMs {
		s.stopCoverage(nowMs, feedTradeGapSilence, ev)
		s.Resumable = true
		return
	}
	for steps := 0; steps < feedTradeMaxIntervals; steps++ {
		end := s.FrontierMs + feedTradeIntervalMs
		if end+feedTradeReorderGraceMs > nowMs || s.LastFrameMs < end {
			break
		}
		iv := s.ensureInterval(s.FrontierMs)
		iv.FinalizedMs = nowMs
		if iv.InvalidMs != 0 {
			iv.Status = feedTradeStatusInvalid
			s.Stats.Invalid++
		} else {
			iv.Status = feedTradeStatusComplete
		}
		iv.Session = s.Session
		s.Stats.Finalized++
		ev.add("interval", intervalEventFields(s, iv))
		s.FrontierMs = end
	}
	s.trim(nowMs)
}

func (s *feedTradeFlowState) retainMs() int64 {
	return s.WindowMs + feedTradeRetainSlackMs
}

func (s *feedTradeFlowState) trim(nowMs int64) {
	floor := nowMs - s.retainMs()
	cut := sort.Search(len(s.Intervals), func(i int) bool { return s.Intervals[i].StartMs >= floor })
	if cut > 0 {
		s.Intervals = append([]*feedTradeInterval(nil), s.Intervals[cut:]...)
	}
	if over := len(s.Intervals) - feedTradeMaxIntervals; over > 0 {
		s.Intervals = append([]*feedTradeInterval(nil), s.Intervals[over:]...)
	}
	kept := s.Gaps[:0]
	for _, g := range s.Gaps {
		if g.EndMs != 0 && g.EndMs < floor {
			continue
		}
		kept = append(kept, g)
	}
	s.Gaps = kept
	identFloor := nowMs - feedTradeIdentityRetain
	n := 0
	for n < len(s.identOrder) {
		tid := s.identOrder[n]
		id, ok := s.idents[tid]
		if ok && id.RecvMs >= identFloor && len(s.idents) <= feedTradeIdentityCap {
			break
		}
		if ok {
			if len(s.idents) > feedTradeIdentityCap && id.StartMs+feedTradeIntervalMs > s.IdentFloorMs {
				s.IdentFloorMs = id.StartMs + feedTradeIntervalMs
			}
			delete(s.idents, tid)
		}
		n++
	}
	if n > 0 {
		s.identOrder = append([]uint64(nil), s.identOrder[n:]...)
	}
}

func (s *feedTradeFlowState) invalidateOpen(atMs int64, reason string) {
	if s.CoverFromMs == 0 {
		return
	}
	for start := s.FrontierMs; start <= feedTradeIntervalStart(atMs); start += feedTradeIntervalMs {
		s.ensureInterval(start).invalidate(atMs, reason)
	}
	for _, iv := range s.Intervals {
		if iv.Status == feedTradeStatusOpen {
			iv.invalidate(atMs, reason)
		}
	}
	if n := len(s.Gaps); n == 0 || s.Gaps[n-1].Reason != reason || s.Gaps[n-1].DetectedMs != atMs {
		s.Gaps = append(s.Gaps, feedObservationGap{StartMs: atMs, EndMs: atMs, DetectedMs: atMs, Reason: reason})
	}
}

type feedTradeOutcome struct {
	Tid      uint64 `json:"tid"`
	Outcome  string `json:"o"`
	Interval int64  `json:"i"`
	Reason   string `json:"why,omitempty"`
}

func (s *feedTradeFlowState) ingestTrade(t feedTrade, recvMs int64, ev *feedTradeEvents) feedTradeOutcome {
	s.Stats.Received++
	start := feedTradeIntervalStart(t.TimeMs)
	out := feedTradeOutcome{Tid: t.Tid, Interval: start}
	digest := t.digest()
	if prior, ok := s.idents[t.Tid]; ok {
		if prior.Digest == digest {
			s.Stats.Duplicates++
			out.Outcome = feedTradeOutcomeDuplicate
			return out
		}
		s.Stats.Conflicts++
		out.Outcome = feedTradeOutcomeConflict
		out.Reason = "same tid with different content"
		for _, st := range []int64{prior.StartMs, start} {
			if iv := s.interval(st); iv != nil {
				if iv.invalidate(recvMs, "conflicting_duplicate") && iv.Status != feedTradeStatusOpen {
					s.Stats.Invalid++
					ev.add("invalidate", map[string]any{"coin": s.Key.Coin, "kind": feedObservationTradeFlow, "start_ms": st, "reason": "conflicting_duplicate", "recv_ms": recvMs})
				}
			} else if s.CoverFromMs != 0 && st >= s.FrontierMs {
				s.ensureInterval(st).invalidate(recvMs, "conflicting_duplicate")
			}
		}
		return out
	}
	if t.TimeMs > recvMs+feedTradeMaxFutureSkewMs {
		s.Stats.Rejected++
		s.Stats.LastReject = fmt.Sprintf("trade %d event time %d is over %dms after its receipt %d", t.Tid, t.TimeMs, feedTradeMaxFutureSkewMs, recvMs)
		out.Outcome = feedTradeOutcomeRejected
		out.Reason = "future_event"
		s.invalidateOpen(recvMs, "future_event")
		return out
	}
	s.idents[t.Tid] = feedTradeIdent{Digest: digest, StartMs: start, RecvMs: recvMs}
	s.identOrder = append(s.identOrder, t.Tid)
	if len(s.idents) > feedTradeIdentityCap {
		s.invalidateOpen(recvMs, feedTradeGapIdentity)
	}
	if prior := s.interval(start); prior != nil && prior.Status != feedTradeStatusOpen {
		s.Stats.Late++
		out.Outcome = feedTradeOutcomeLate
		if prior.invalidate(recvMs, "late_event") {
			s.Stats.Invalid++
			ev.add("invalidate", map[string]any{"coin": s.Key.Coin, "kind": feedObservationTradeFlow, "start_ms": start, "reason": "late_event", "recv_ms": recvMs})
		}
		return out
	}
	switch {
	case s.CoverFromMs == 0 || start < s.CoverFromMs:
		s.Stats.Uncovered++
		out.Outcome = feedTradeOutcomeUncovered
		return out
	case start < s.FrontierMs:
		s.Stats.Late++
		out.Outcome = feedTradeOutcomeLate
		out.Reason = "interval_evicted"
		return out
	}
	iv := s.ensureInterval(start)
	var ok bool
	if t.Side == "B" {
		iv.BuyUnits, ok = addFeedUnits(iv.BuyUnits, t.Units)
	} else {
		iv.SellUnits, ok = addFeedUnits(iv.SellUnits, t.Units)
	}
	if !ok {
		iv.invalidate(recvMs, "volume_overflow")
	}
	addFeedTradeHash(&iv.Hash, digest)
	iv.Trades++
	if iv.FirstRecvMs == 0 {
		iv.FirstRecvMs = recvMs
	}
	iv.LastRecvMs = recvMs
	s.Stats.Accepted++
	out.Outcome = feedTradeOutcomeAccepted
	return out
}

func (o *marketFeedOwner) TradeFlowCoins() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make([]string, 0, len(o.tradeFlows))
	for k := range o.tradeFlows {
		out = append(out, k.Coin)
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) emitTradeEventsLocked(ev feedTradeEvents, session uint64) {
	if o.recorder == nil {
		return
	}
	for _, e := range ev {
		recv, _ := e.Fields["recv_ms"].(int64)
		o.recorder.offer(observationRecord{Kind: e.Kind, RecvMs: recv, Session: session, Extra: e.Fields})
	}
}

func (o *marketFeedOwner) advanceTradeFlowsLocked(nowMs int64, ev *feedTradeEvents) {
	keys := make([]feedObservationKey, 0, len(o.tradeFlows))
	for k := range o.tradeFlows {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	for _, k := range keys {
		o.tradeFlows[k].advance(nowMs, ev)
	}
}

func (o *marketFeedOwner) AdvanceTradeFlows(now time.Time) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.tickTradeFlowsLocked(now.UTC().UnixMilli())
}

func (o *marketFeedOwner) tickTradeFlowsLocked(nowMs int64) {
	var ev feedTradeEvents
	o.advanceTradeFlowsLocked(nowMs, &ev)
	if len(ev) > 0 && o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "tick", RecvMs: nowMs, Session: o.obsSession, Extra: map[string]any{"recv_ms": nowMs}})
	}
	o.emitTradeEventsLocked(ev, o.obsSession)
}

func (o *marketFeedOwner) IngestTradeSubscription(coin string, recvAt time.Time, raw []byte) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.tradeFlows[tradeFlowKeyFor(coin)]
	if st == nil {
		return
	}
	var ev feedTradeEvents
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "sub", RecvMs: recvMs, Session: o.obsSession, Extra: map[string]any{
			"coin": coin, "kind": feedObservationTradeFlow, "raw": string(raw), "recv_ms": recvMs,
		}})
	}
	if !o.connected {
		return
	}
	st.startCoverage(recvMs, o.obsSession, &ev)
	o.emitTradeEventsLocked(ev, o.obsSession)
}

func (o *marketFeedOwner) IngestTradesFrame(data json.RawMessage, recvAt time.Time, raw []byte) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.tradeFlows) == 0 {
		return
	}
	var ev feedTradeEvents
	elems, err := parseFeedTradesFrame(data)
	parsed := make([]feedTradeParse, 0, len(elems))
	coins := map[string]bool{}
	for _, e := range elems {
		p := parseFeedTradeElement(e)
		parsed = append(parsed, p)
		if p.Trade.Coin != "" {
			coins[p.Trade.Coin] = true
		}
	}
	frameCoin := ""
	if len(coins) == 1 {
		for c := range coins {
			frameCoin = c
		}
	}
	record := map[string]any{"raw": string(raw), "recv_ms": recvMs, "coin": frameCoin}
	var outcomes []feedTradeOutcome
	switch {
	case err != nil || len(coins) == 0:
		reason := "trades frame names no coin"
		if err != nil {
			reason = err.Error()
		}
		record["error"] = reason
		for _, k := range sortedTradeFlowKeys(o.tradeFlows) {
			st := o.tradeFlows[k]
			st.Stats.Rejected++
			st.Stats.LastReject = reason
			st.invalidateOpen(recvMs, feedTradeGapMalformed)
		}
		o.logf("[feed] rejected a trades frame: %s", reason)
	default:
		for _, p := range parsed {
			st := o.tradeFlows[tradeFlowKeyFor(p.Trade.Coin)]
			if st == nil {
				outcomes = append(outcomes, feedTradeOutcome{Tid: p.Tid, Outcome: feedTradeOutcomeRejected, Reason: "untracked_coin"})
				continue
			}
			if p.Err != nil {
				st.Stats.Received++
				st.Stats.Rejected++
				st.Stats.LastReject = p.Err.Error()
				st.invalidateOpen(recvMs, feedTradeGapMalformed)
				outcomes = append(outcomes, feedTradeOutcome{Tid: p.Tid, Outcome: feedTradeOutcomeRejected, Reason: p.Err.Error()})
				continue
			}
			if recvMs < st.LastRecvMs {
				st.Stats.Received++
				st.Stats.Rejected++
				st.Stats.LastReject = fmt.Sprintf("clock_regression: receipt %d is before the previous receipt %d", recvMs, st.LastRecvMs)
				st.invalidateOpen(recvMs, feedTradeGapClock)
				outcomes = append(outcomes, feedTradeOutcome{Tid: p.Tid, Outcome: feedTradeOutcomeRejected, Reason: feedTradeGapClock, Interval: feedTradeIntervalStart(p.Trade.TimeMs)})
				continue
			}
			outcomes = append(outcomes, st.ingestTrade(p.Trade, recvMs, &ev))
		}
		frameCoins := make([]string, 0, len(coins))
		for c := range coins {
			frameCoins = append(frameCoins, c)
		}
		sort.Strings(frameCoins)
		for _, c := range frameCoins {
			if st := o.tradeFlows[tradeFlowKeyFor(c)]; st != nil {
				st.Stats.Frames++
				if recvMs >= st.LastRecvMs {
					st.LastRecvMs = recvMs
					if st.CoverFromMs != 0 {
						st.LastFrameMs = recvMs
					} else if st.Resumable && o.connected && st.Session == o.obsSession {
						st.beginCoverage(recvMs, "resumed", &ev)
					}
				}
			}
		}
	}
	if outcomes == nil {
		outcomes = []feedTradeOutcome{}
	}
	record["outcomes"] = outcomes
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "trades", RecvMs: recvMs, Session: o.obsSession, Extra: record})
	}
	o.advanceTradeFlowsLocked(recvMs, &ev)
	o.emitTradeEventsLocked(ev, o.obsSession)
}

func sortedTradeFlowKeys(m map[feedObservationKey]*feedTradeFlowState) []feedObservationKey {
	keys := make([]feedObservationKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	return keys
}

func (o *marketFeedOwner) markTradeFlowsDisconnectedLocked(atMs int64) {
	var ev feedTradeEvents
	for _, k := range sortedTradeFlowKeys(o.tradeFlows) {
		o.tradeFlows[k].stopCoverage(atMs, feedTradeGapDisconnected, &ev)
	}
	o.emitTradeEventsLocked(ev, o.obsSession)
}

type feedTradeFlowReadiness struct {
	Key          feedObservationKey
	Status       string
	Detail       string
	Ready        bool
	Intervals    int
	Complete     int
	CoveredMs    int64
	WindowMs     int64
	CoverFromMs  int64
	FrontierMs   int64
	OpenGap      bool
	Stats        feedTradeFlowStats
	SourceDetail string
}

func tradeFlowReadiness(s *feedTradeFlowState, nowMs int64) feedTradeFlowReadiness {
	out := feedTradeFlowReadiness{
		Key: s.Key, Intervals: len(s.Intervals), WindowMs: s.WindowMs, CoverFromMs: s.CoverFromMs,
		FrontierMs: s.FrontierMs, Stats: s.Stats, SourceDetail: feedTradeSourceHLWS + "/" + feedTradeUnitsBase + "/" + feedTradeTimeBasis,
	}
	for _, iv := range s.Intervals {
		if iv.Status == feedTradeStatusComplete && iv.InvalidMs == 0 {
			out.Complete++
		}
	}
	var run int64
	for i := len(s.Intervals) - 1; i >= 0; i-- {
		iv := s.Intervals[i]
		if iv.Status == feedTradeStatusOpen {
			continue
		}
		if iv.Status != feedTradeStatusComplete || iv.InvalidMs != 0 {
			break
		}
		if i+1 < len(s.Intervals) && s.Intervals[i+1].Status != feedTradeStatusOpen && s.Intervals[i+1].StartMs != iv.StartMs+feedTradeIntervalMs {
			break
		}
		run += feedTradeIntervalMs
	}
	out.CoveredMs = run
	if g := s.openGap(); g != nil && s.CoverFromMs == 0 {
		out.OpenGap = true
		out.Status = feedObservationStatusGap
		out.Detail = fmt.Sprintf("%s since %d", g.Reason, g.StartMs)
		return out
	}
	if s.CoverFromMs == 0 {
		out.Status = feedTradeStatusNoCoverage
		out.Detail = "no acknowledged trades subscription in this socket session"
		return out
	}
	if run < s.WindowMs {
		out.Status = feedObservationStatusWarming
		out.Detail = fmt.Sprintf("%dms of contiguous complete intervals, %dms required", run, s.WindowMs)
		return out
	}
	out.Status = feedObservationStatusReady
	out.Ready = true
	return out
}

func (o *marketFeedOwner) TradeFlowReadiness() []feedTradeFlowReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	out := make([]feedTradeFlowReadiness, 0, len(o.tradeFlows))
	for _, k := range sortedTradeFlowKeys(o.tradeFlows) {
		out = append(out, tradeFlowReadiness(o.tradeFlows[k], nowMs))
	}
	return out
}

func (o *marketFeedOwner) trackTradeFlows(coins []string, windowMs int64) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	needs := make(map[feedObservationKey]int64, len(coins))
	for _, c := range coins {
		needs[tradeFlowKeyFor(c)] = windowMs
	}
	o.applyObservationNeedsLocked(needs)
	o.subVersion++
}

type marketSnapshotTradeInterval struct {
	StartMs     int64
	BuyUnits    int64
	SellUnits   int64
	Trades      int
	Hash        string
	Session     uint64
	FinalizedMs int64
	Status      string
	Reason      string
	InvalidMs   int64
}

type marketSnapshotTradeFlow struct {
	Key       feedObservationKey
	WindowMs  int64
	CutoffMs  int64
	Intervals []marketSnapshotTradeInterval
	Gaps      []feedObservationGap
	Readiness feedTradeFlowReadiness
}

func freezeTradeFlow(st *feedTradeFlowState, windowMs, cutoffMs, nowMs int64) *marketSnapshotTradeFlow {
	start := cutoffMs - windowMs
	out := &marketSnapshotTradeFlow{
		Key: st.Key, WindowMs: windowMs, CutoffMs: cutoffMs,
		Intervals: []marketSnapshotTradeInterval{}, Gaps: []feedObservationGap{},
		Readiness: tradeFlowReadiness(st, nowMs),
	}
	for _, iv := range st.Intervals {
		if iv.FinalizedMs == 0 || iv.FinalizedMs > cutoffMs || iv.StartMs < start || iv.StartMs+feedTradeIntervalMs > cutoffMs {
			continue
		}
		snap := marketSnapshotTradeInterval{
			StartMs: iv.StartMs, BuyUnits: iv.BuyUnits, SellUnits: iv.SellUnits, Trades: iv.Trades,
			Hash: hex.EncodeToString(iv.Hash[:]), Session: iv.Session, FinalizedMs: iv.FinalizedMs, Status: feedTradeStatusComplete,
		}
		if iv.InvalidMs != 0 && iv.InvalidMs <= cutoffMs {
			snap.Status, snap.Reason, snap.InvalidMs = feedTradeStatusInvalid, iv.Reason, iv.InvalidMs
		}
		out.Intervals = append(out.Intervals, snap)
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

type marketTradeInterval struct {
	StartMs     int64  `json:"start_ms"`
	EndMs       int64  `json:"end_ms"`
	Buy         string `json:"buy"`
	Sell        string `json:"sell"`
	Total       string `json:"total"`
	Trades      int    `json:"trades"`
	Hash        string `json:"records_hash"`
	Session     uint64 `json:"session"`
	FinalizedMs int64  `json:"finalized_ms"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	InvalidMs   *int64 `json:"invalid_ms"`
}

type marketTradeFlowPayload struct {
	Kind           string                 `json:"kind"`
	Coin           string                 `json:"coin"`
	Source         string                 `json:"source"`
	Units          string                 `json:"units"`
	TimeBasis      string                 `json:"time_basis"`
	Identity       string                 `json:"identity"`
	SideEvidence   string                 `json:"side_evidence"`
	IntervalMs     int64                  `json:"interval_ms"`
	ReorderGraceMs int64                  `json:"reorder_grace_ms"`
	MaxEventGapMs  int64                  `json:"max_event_gap_ms"`
	DecimalScale   int                    `json:"decimal_scale"`
	WindowMs       int64                  `json:"window_ms"`
	CutoffMs       int64                  `json:"cutoff_ms"`
	BarIntervalMs  int64                  `json:"bar_interval_ms"`
	SnapshotID     string                 `json:"snapshot_id"`
	Available      bool                   `json:"available"`
	Reason         string                 `json:"reason,omitempty"`
	FeedStatus     string                 `json:"feed_status,omitempty"`
	Intervals      []marketTradeInterval  `json:"intervals"`
	Gaps           []marketObservationGap `json:"gaps"`
}

func newMarketTradeFlowPayload(coin string, windowMs, barIntervalMs int64) marketTradeFlowPayload {
	return marketTradeFlowPayload{
		Kind: feedObservationTradeFlow, Coin: coin, Source: feedTradeSourceHLWS, Units: feedTradeUnitsBase,
		TimeBasis: feedTradeTimeBasis, Identity: feedTradeIdentity, SideEvidence: feedTradeSideEvidence,
		IntervalMs: feedTradeIntervalMs, ReorderGraceMs: feedTradeReorderGraceMs, MaxEventGapMs: feedTradeMaxEventGapMs,
		DecimalScale: feedTradeDecimalScale, WindowMs: windowMs, BarIntervalMs: barIntervalMs,
		Intervals: []marketTradeInterval{}, Gaps: []marketObservationGap{},
	}
}

func (s *marketSnapshot) tradeFlowPayload(key feedObservationKey, windowMs, barIntervalMs int64) marketTradeFlowPayload {
	out := newMarketTradeFlowPayload(key.Coin, windowMs, barIntervalMs)
	if s == nil {
		out.Reason = "no sealed market snapshot"
		return out
	}
	out.SnapshotID = s.EvaluationID
	entry, ok := s.tradeFlows[key]
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
	for _, iv := range entry.Intervals {
		if iv.StartMs < start {
			continue
		}
		total, _ := addFeedUnits(iv.BuyUnits, iv.SellUnits)
		mi := marketTradeInterval{
			StartMs: iv.StartMs, EndMs: iv.StartMs + feedTradeIntervalMs, Buy: formatFeedUnits(iv.BuyUnits),
			Sell: formatFeedUnits(iv.SellUnits), Total: formatFeedUnits(total), Trades: iv.Trades, Hash: iv.Hash,
			Session: iv.Session, FinalizedMs: iv.FinalizedMs, Status: iv.Status, Reason: iv.Reason,
		}
		if iv.InvalidMs != 0 {
			v := iv.InvalidMs
			mi.InvalidMs = &v
		}
		out.Intervals = append(out.Intervals, mi)
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

func attachTradeFlowPayloads(payload *marketPayload, s *marketSnapshot, needs map[feedObservationKey]int64, barIntervalMs int64) {
	if payload == nil {
		return
	}
	var keys []feedObservationKey
	for k := range needs {
		if k.Kind == feedObservationTradeFlow {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sortFeedObservationKeys(keys)
	payload.TradeFlows = make(map[string]marketTradeFlowPayload, len(keys))
	for _, k := range keys {
		payload.TradeFlows[k.PayloadID()] = s.tradeFlowPayload(k, needs[k], barIntervalMs)
	}
}

func cvdSessionWindowMs(sc StrategyConfig, intervalMs int64) (int64, error) {
	hours, err := openInterestParamInt(sc.OpenStrategy.Params, "session_hours", cvdDefaultSessionHours)
	if err != nil {
		return 0, err
	}
	if 24%hours != 0 {
		return 0, fmt.Errorf("session_hours %d does not divide 24", hours)
	}
	window := hours*3_600_000 + intervalMs + feedTradeIntervalMs
	if window > feedTradeMaxWindowMs {
		return 0, fmt.Errorf("trade-flow window %dms (session_hours %d) is over the %dms retention cap", window, hours, feedTradeMaxWindowMs)
	}
	return window, nil
}

func feedStrategyUsesTradeFlow(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == cumulativeVolumeDeltaStrategyName
}
