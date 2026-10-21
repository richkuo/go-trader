package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	feedObservationL2Book     = "l2_book"
	feedBookSource            = "hyperliquid_ws_l2Book"
	feedBookContract          = "hyperliquid-l2book-native-slow/1635-v1"
	feedBookTimeSemantics     = "source_event_ms+first_receipt_ms/1635-v1"
	feedBookIdentitySemantics = "sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1"
	feedBookIdentityTag       = "go-trader-l2book-identity/v1"

	feedBookMaxLevels       = 20
	feedBookMaxObservations = 4096
	feedBookMaxWindowMs     = int64(900_000)
	feedBookMaxMarkers      = 64
	feedBookMaxFutureSkewMs = int64(250)
	feedBookMaxLagMs        = int64(5_000)
	feedBookMaxGapMs        = int64(10_000)
	feedBookMaxKeys         = 16
	feedBookMaxFrameBytes   = 16 << 10
	feedBookEnvelopeBudget  = 4 << 20
	feedBookSealBudget      = 16 << 20
	feedBookMemoryBudget    = 64 << 20
	feedBookDecimalMaxLen   = 32
	feedBookDecimalMaxFrac  = 18
	feedBookMaxOrders       = uint64(math.MaxUint32)
	feedBookRetainSlackMs   = int64(60_000)

	feedBookSingleReserveBytes = 4 << 10
	feedBookBatchReserveBytes  = 256 << 10

	feedBookMaxHostLen     = 256
	feedBookMaxNamespace   = 32
	feedBookMaxCoinLen     = 64
	feedBookMaxReasonLen   = 240
	feedBookMaxInstanceLen = 128

	feedBookOutcomeAccepted   = "accepted"
	feedBookOutcomeDuplicate  = "duplicate"
	feedBookOutcomeConflict   = "conflict"
	feedBookOutcomeOutOfOrder = "out_of_order"
	feedBookOutcomeRejected   = "rejected"
	feedBookOutcomeUncovered  = "uncovered"
	feedBookOutcomeUntracked  = "untracked"

	feedBookMarkerEpoch        = "epoch_start"
	feedBookMarkerSubscribed   = "subscribed"
	feedBookMarkerSubFailed    = "subscription_failed"
	feedBookMarkerDisconnected = "disconnected"
	feedBookMarkerInvalid      = "invalid_frame"
	feedBookMarkerConflict     = "conflict"
	feedBookMarkerOutOfOrder   = "out_of_order"
	feedBookMarkerClock        = "clock_regression"
	feedBookMarkerSilence      = "silence"
	feedBookMarkerEvidenceLoss = "evidence_loss"
	feedBookMarkerDepthRaised  = "depth_raised"

	feedBookStatusNoCoverage = "no_coverage"

	feedBookRefusedCapacity = "book_capacity_refused"

	feedEstimateBookRecordBase  = 320
	feedEstimateBookLevelBytes  = 96
	feedEstimateBookMarkerBytes = 480
	feedEstimateBookKeyBytes    = 1536
	feedBookMemRecordBase       = 320
	feedBookMemLevelBytes       = 112
)

const feedBookTickEvery = time.Second

var feedBookMarkerReasons = map[string]bool{
	feedBookMarkerEpoch: true, feedBookMarkerSubscribed: true, feedBookMarkerSubFailed: true, feedBookMarkerDisconnected: true,
	feedBookMarkerInvalid: true, feedBookMarkerConflict: true, feedBookMarkerOutOfOrder: true, feedBookMarkerClock: true,
	feedBookMarkerSilence: true, feedBookMarkerEvidenceLoss: true, feedBookMarkerDepthRaised: true,
}

func l2BookKeyFor(coin string) feedObservationKey {
	return feedObservationKey{Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, Kind: feedObservationL2Book}
}

type feedBookNeed struct {
	Source            string
	Contract          string
	TimeSemantics     string
	IdentitySemantics string
	WindowMs          int64
	Capacity          int
	Depth             int
}

func contractBookNeed(windowMs int64, capacity, depth int) feedBookNeed {
	return feedBookNeed{Source: feedBookSource, Contract: feedBookContract, TimeSemantics: feedBookTimeSemantics,
		IdentitySemantics: feedBookIdentitySemantics, WindowMs: windowMs, Capacity: capacity, Depth: depth}
}

func (n feedBookNeed) sameSemantics(o feedBookNeed) bool {
	return n.Source == o.Source && n.Contract == o.Contract && n.TimeSemantics == o.TimeSemantics && n.IdentitySemantics == o.IdentitySemantics
}

func (n feedBookNeed) validate() error {
	switch {
	case n.Source != feedBookSource || n.Contract != feedBookContract || n.TimeSemantics != feedBookTimeSemantics || n.IdentitySemantics != feedBookIdentitySemantics:
		return fmt.Errorf("book need declares source %q contract %q; this build supplies only %q %q", n.Source, n.Contract, feedBookSource, feedBookContract)
	case n.WindowMs <= 0 || n.WindowMs > feedBookMaxWindowMs:
		return fmt.Errorf("book window %dms is outside (0, %d]", n.WindowMs, feedBookMaxWindowMs)
	case n.Capacity <= 0 || n.Capacity > feedBookMaxObservations:
		return fmt.Errorf("book capacity %d observations is outside [1, %d]", n.Capacity, feedBookMaxObservations)
	case n.Depth <= 0 || n.Depth > feedBookMaxLevels:
		return fmt.Errorf("book depth %d levels is outside [1, %d]", n.Depth, feedBookMaxLevels)
	}
	return nil
}

func mergeBookNeed(a, b feedBookNeed) (feedBookNeed, error) {
	if !a.sameSemantics(b) {
		return feedBookNeed{}, fmt.Errorf("book needs disagree on source or semantics (%q %q versus %q %q)", a.Source, a.Contract, b.Source, b.Contract)
	}
	out := a
	if b.WindowMs > out.WindowMs {
		out.WindowMs = b.WindowMs
	}
	if b.Capacity > out.Capacity {
		out.Capacity = b.Capacity
	}
	if b.Depth > out.Depth {
		out.Depth = b.Depth
	}
	return out, nil
}

func addBookNeed(dst map[feedObservationKey]feedBookNeed, key feedObservationKey, need feedBookNeed, conflicts *[]string) {
	existing, ok := dst[key]
	if !ok {
		dst[key] = need
		return
	}
	merged, err := mergeBookNeed(existing, need)
	if err != nil {
		if conflicts != nil {
			*conflicts = append(*conflicts, fmt.Sprintf("%s: %v", key.PayloadID(), err))
		}
		return
	}
	dst[key] = merged
}

func sortedBookNeedKeys(m map[feedObservationKey]feedBookNeed) []feedObservationKey {
	keys := make([]feedObservationKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	return keys
}

func feedBookRecordBytes(depth int) int {
	return feedEstimateBookRecordBase + 2*depth*feedEstimateBookLevelBytes
}

func feedBookSectionBytes(need feedBookNeed) int {
	return feedEstimateBookKeyBytes + need.Capacity*feedBookRecordBytes(need.Depth) + feedBookMaxMarkers*feedEstimateBookMarkerBytes
}

func feedBookMemoryBytes(need feedBookNeed) int {
	return need.Capacity * (feedBookMemRecordBase + 2*need.Depth*feedBookMemLevelBytes)
}

func bookNeedsAdmissionErrors(books map[feedObservationKey]feedBookNeed) []string {
	var errs []string
	if len(books) > feedBookMaxKeys {
		errs = append(errs, fmt.Sprintf("%d l2_book keys exceed the %d-key bound", len(books), feedBookMaxKeys))
	}
	seal, mem := 0, 0
	for _, k := range sortedBookNeedKeys(books) {
		need := books[k]
		if err := need.validate(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", k.PayloadID(), err))
			continue
		}
		section := feedBookSectionBytes(need)
		if section > feedBookEnvelopeBudget {
			errs = append(errs, fmt.Sprintf("%s: window %dms, %d observations at %d levels need up to %d book bytes per market envelope, over the %d-byte budget",
				k.PayloadID(), need.WindowMs, need.Capacity, need.Depth, section, feedBookEnvelopeBudget))
		}
		seal += section
		mem += feedBookMemoryBytes(need)
	}
	if seal > feedBookSealBudget {
		errs = append(errs, fmt.Sprintf("the l2_book needs total up to %d seal bytes, over the %d-byte budget", seal, feedBookSealBudget))
	}
	if mem > feedBookMemoryBudget {
		errs = append(errs, fmt.Sprintf("the l2_book needs total up to %d owner-memory bytes, over the %d-byte budget", mem, feedBookMemoryBudget))
	}
	return errs
}

func bookUnionError(r feedRequirements) error {
	errs := append([]string(nil), r.BookConflicts...)
	errs = append(errs, bookNeedsAdmissionErrors(r.Books)...)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("l2_book requirements refused: %s", strings.Join(errs, "; "))
}

type feedBookNeedFunc func(sc StrategyConfig, intervalMs int64) (feedBookNeed, error)

var feedBookStrategyNeeds = map[string]feedBookNeedFunc{}

func feedStrategyBookNeed(sc StrategyConfig, intervalMs int64) (feedBookNeed, bool, error) {
	fn, ok := feedBookStrategyNeeds[strings.TrimSpace(effectiveOpenStrategy(sc))]
	if !ok {
		return feedBookNeed{}, false, nil
	}
	need, err := fn(sc, intervalMs)
	if err != nil {
		return feedBookNeed{}, true, err
	}
	return need, true, need.validate()
}

func bookStrategyIDs(cfg *Config) []string {
	var ids []string
	for _, sc := range cfg.Strategies {
		if !feedScopedStrategy(sc) {
			continue
		}
		if _, ok := feedBookStrategyNeeds[strings.TrimSpace(effectiveOpenStrategy(sc))]; ok {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

type feedBookLevel struct {
	Px string
	Sz string
	N  uint64
}

func (l feedBookLevel) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]any{l.Px, l.Sz, l.N})
}

func (l *feedBookLevel) UnmarshalJSON(b []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil || len(parts) != 3 {
		return errors.New("book level must be a [px, sz, n] array")
	}
	if err := json.Unmarshal(parts[0], &l.Px); err != nil {
		return errors.New("book level px must be a string")
	}
	if err := json.Unmarshal(parts[1], &l.Sz); err != nil {
		return errors.New("book level sz must be a string")
	}
	dec := json.NewDecoder(bytes.NewReader(parts[2]))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return errors.New("book level n must be an integer")
	}
	v, err := strconv.ParseUint(n.String(), 10, 64)
	if err != nil {
		return errors.New("book level n must be an unsigned integer")
	}
	l.N = v
	return nil
}

func canonicalBookDecimal(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > feedBookDecimalMaxLen {
		return "", fmt.Errorf("decimal of %d characters is empty or longer than %d", len(raw), feedBookDecimalMaxLen)
	}
	intPart, frac, hasDot := strings.Cut(raw, ".")
	if intPart == "" || (hasDot && frac == "") || strings.Contains(frac, ".") {
		return "", fmt.Errorf("decimal %q is malformed", raw)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '.' && (c < '0' || c > '9') {
			return "", fmt.Errorf("decimal %q is not a plain positive ASCII decimal", raw)
		}
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > feedBookDecimalMaxFrac {
		return "", fmt.Errorf("decimal %q has more than %d fractional digits", raw, feedBookDecimalMaxFrac)
	}
	out := intPart
	if frac != "" {
		out += "." + frac
	}
	if out == "0" {
		return "", fmt.Errorf("decimal %q is not positive", raw)
	}
	return out, nil
}

func bookDecimalRat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

func validateBookSides(bids, asks []feedBookLevel) error {
	if len(bids) == 0 || len(asks) == 0 {
		return errors.New("empty_side: a book side carries no level")
	}
	if len(bids) > feedBookMaxLevels || len(asks) > feedBookMaxLevels {
		return fmt.Errorf("too_many_levels: %d bids and %d asks, over %d", len(bids), len(asks), feedBookMaxLevels)
	}
	for _, side := range []struct {
		name   string
		levels []feedBookLevel
	}{{"bid", bids}, {"ask", asks}} {
		for i, l := range side.levels {
			for _, d := range []struct{ name, v string }{{"px", l.Px}, {"sz", l.Sz}} {
				c, err := canonicalBookDecimal(d.v)
				if err != nil || c != d.v {
					return fmt.Errorf("invalid_level: %s %d %s %q is not a canonical positive decimal", side.name, i, d.name, d.v)
				}
			}
			if l.N == 0 || l.N > feedBookMaxOrders {
				return fmt.Errorf("invalid_level: %s %d order count %d is outside [1, %d]", side.name, i, l.N, feedBookMaxOrders)
			}
		}
	}
	for i := 1; i < len(bids); i++ {
		if bookDecimalRat(bids[i].Px).Cmp(bookDecimalRat(bids[i-1].Px)) >= 0 {
			return fmt.Errorf("unsorted_side: bid %d is not strictly below bid %d", i, i-1)
		}
	}
	for i := 1; i < len(asks); i++ {
		if bookDecimalRat(asks[i].Px).Cmp(bookDecimalRat(asks[i-1].Px)) <= 0 {
			return fmt.Errorf("unsorted_side: ask %d is not strictly above ask %d", i, i-1)
		}
	}
	if bookDecimalRat(bids[0].Px).Cmp(bookDecimalRat(asks[0].Px)) >= 0 {
		return errors.New("crossed_book: the best bid is not strictly below the best ask")
	}
	return nil
}

func lpField(b *bytes.Buffer, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
	b.WriteByte(';')
}

func bookIdentity(key feedObservationKey, eventMs int64, bids, asks []feedBookLevel) string {
	var b bytes.Buffer
	lpField(&b, feedBookIdentityTag)
	lpField(&b, key.Host)
	lpField(&b, key.Namespace)
	lpField(&b, key.Coin)
	lpField(&b, strconv.FormatInt(eventMs, 10))
	for _, side := range [][]feedBookLevel{bids, asks} {
		lpField(&b, strconv.Itoa(len(side)))
		for _, l := range side {
			lpField(&b, l.Px)
			lpField(&b, l.Sz)
			lpField(&b, strconv.FormatUint(l.N, 10))
		}
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

type feedSocketBookLevel struct {
	Px string      `json:"px"`
	Sz string      `json:"sz"`
	N  json.Number `json:"n"`
}

type feedSocketBook struct {
	Coin   string                  `json:"coin"`
	Time   json.Number             `json:"time"`
	Levels [][]feedSocketBookLevel `json:"levels"`
}

type feedBookParse struct {
	Coin     string
	EventMs  int64
	Bids     []feedBookLevel
	Asks     []feedBookLevel
	Identity string
	Err      error
}

func parseBookLevels(in []feedSocketBookLevel, side string) ([]feedBookLevel, error) {
	if len(in) > feedBookMaxLevels {
		return nil, fmt.Errorf("too_many_levels: %s side carries %d levels, over %d", side, len(in), feedBookMaxLevels)
	}
	out := make([]feedBookLevel, 0, len(in))
	for i, l := range in {
		px, err := canonicalBookDecimal(l.Px)
		if err != nil {
			return nil, fmt.Errorf("invalid_level: %s %d px: %v", side, i, err)
		}
		sz, err := canonicalBookDecimal(l.Sz)
		if err != nil {
			return nil, fmt.Errorf("invalid_level: %s %d sz: %v", side, i, err)
		}
		n, err := strconv.ParseUint(l.N.String(), 10, 64)
		if err != nil || n == 0 || n > feedBookMaxOrders {
			return nil, fmt.Errorf("invalid_level: %s %d order count %q is not an integer in [1, %d]", side, i, l.N.String(), feedBookMaxOrders)
		}
		out = append(out, feedBookLevel{Px: px, Sz: sz, N: n})
	}
	return out, nil
}

func parseBookFrame(key func(string) feedObservationKey, data json.RawMessage) feedBookParse {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var msg feedSocketBook
	if err := dec.Decode(&msg); err != nil {
		return feedBookParse{Err: errors.New("malformed: the l2Book data does not decode")}
	}
	out := feedBookParse{Coin: msg.Coin}
	if out.Coin == "" || len(out.Coin) > feedBookMaxCoinLen || strings.TrimSpace(out.Coin) != out.Coin {
		out.Coin = ""
		out.Err = errors.New("malformed: the l2Book frame carries no usable coin")
		return out
	}
	eventMs, err := strconv.ParseInt(msg.Time.String(), 10, 64)
	if err != nil || eventMs <= 0 {
		out.Err = fmt.Errorf("malformed: time %q is not a positive millisecond timestamp", msg.Time.String())
		return out
	}
	out.EventMs = eventMs
	if len(msg.Levels) != 2 {
		out.Err = fmt.Errorf("malformed: %d level sides, want 2", len(msg.Levels))
		return out
	}
	bids, err := parseBookLevels(msg.Levels[0], "bid")
	if err != nil {
		out.Err = err
		return out
	}
	asks, err := parseBookLevels(msg.Levels[1], "ask")
	if err != nil {
		out.Err = err
		return out
	}
	if err := validateBookSides(bids, asks); err != nil {
		out.Err = err
		return out
	}
	out.Bids, out.Asks = bids, asks
	out.Identity = bookIdentity(key(out.Coin), eventMs, bids, asks)
	return out
}

type feedBookObservation struct {
	Seq      uint64
	EventMs  int64
	RecvMs   int64
	Session  uint64
	Identity string
	BidDepth int
	AskDepth int
	Bids     []feedBookLevel
	Asks     []feedBookLevel
}

type feedBookMarker struct {
	Seq     uint64
	AtMs    int64
	Reason  string
	Session uint64
	Detail  string
}

type feedBookStats struct {
	Frames     uint64
	Accepted   uint64
	Duplicates uint64
	Conflicts  uint64
	OutOfOrder uint64
	Rejected   uint64
	Uncovered  uint64
	Markers    uint64
	Evicted    uint64
	LastReject string
}

type feedBookState struct {
	Key            feedObservationKey
	Need           feedBookNeed
	Obs            []feedBookObservation
	Markers        []feedBookMarker
	Covered        bool
	Session        uint64
	HighRecvMs     int64
	HighEventMs    int64
	LastAcceptMs   int64
	SilenceMarked  bool
	RetainedFromMs int64
	DepthFromMs    int64
	StartedMs      int64
	Stats          feedBookStats
}

type feedBookEvent struct {
	Kind   string
	RecvMs int64
	Fields map[string]any
}

type feedBookEvents []feedBookEvent

func boundedReason(s string) string {
	if len(s) > feedBookMaxReasonLen {
		s = s[:feedBookMaxReasonLen]
	}
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			b[i] = '?'
		}
	}
	return string(b)
}

func boundedBookInstance(s string) string {
	s = boundedReason(s)
	if len(s) > feedBookMaxInstanceLen {
		s = s[:feedBookMaxInstanceLen]
	}
	return s
}

func (o *marketFeedOwner) nextBookSeq() uint64 {
	o.bookSeq++
	return o.bookSeq
}

func (o *marketFeedOwner) addBookMarkerLocked(st *feedBookState, atMs int64, reason, detail string, ev *feedBookEvents) {
	m := feedBookMarker{Seq: o.nextBookSeq(), AtMs: atMs, Reason: reason, Session: st.Session, Detail: boundedReason(detail)}
	st.Markers = append(st.Markers, m)
	st.Stats.Markers++
	if ev != nil {
		*ev = append(*ev, feedBookEvent{Kind: "book_marker", RecvMs: atMs, Fields: map[string]any{
			"coin": st.Key.Coin, "kind": feedObservationL2Book, "seq": m.Seq, "at_ms": m.AtMs, "reason": m.Reason,
			"marker_session": m.Session, "detail": m.Detail, "recv_ms": atMs}})
	}
	if len(st.Markers) > feedBookMaxMarkers {
		o.coalesceBookMarkersLocked(st, atMs, ev)
	}
}

func (o *marketFeedOwner) coalesceBookMarkersLocked(st *feedBookState, atMs int64, ev *feedBookEvents) {
	drop := len(st.Markers) - feedBookMaxMarkers + 1
	cut := st.Markers[drop-1]
	keep := sort.Search(len(st.Obs), func(i int) bool { return st.Obs[i].Seq > cut.Seq })
	if keep > 0 {
		st.Stats.Evicted += uint64(keep)
		st.Obs = append([]feedBookObservation(nil), st.Obs[keep:]...)
	}
	st.Markers = append([]feedBookMarker(nil), st.Markers[drop:]...)
	if cut.AtMs+1 > st.RetainedFromMs {
		st.RetainedFromMs = cut.AtMs + 1
	}
	m := feedBookMarker{Seq: o.nextBookSeq(), AtMs: atMs, Reason: feedBookMarkerEvidenceLoss, Session: st.Session,
		Detail: boundedReason(fmt.Sprintf("%d markers through seq %d coalesced; history is complete only from %d", drop, cut.Seq, st.RetainedFromMs))}
	st.Markers = append(st.Markers, m)
	st.Stats.Markers++
	if ev != nil {
		*ev = append(*ev, feedBookEvent{Kind: "book_marker", RecvMs: atMs, Fields: map[string]any{
			"coin": st.Key.Coin, "kind": feedObservationL2Book, "seq": m.Seq, "at_ms": m.AtMs, "reason": m.Reason,
			"marker_session": m.Session, "detail": m.Detail, "recv_ms": atMs}})
	}
}

func (s *feedBookState) retainFloor(nowMs int64) int64 {
	return nowMs - s.Need.WindowMs - feedBookRetainSlackMs
}

func (s *feedBookState) trim(nowMs int64) {
	floor := s.retainFloor(nowMs)
	cut := sort.Search(len(s.Obs), func(i int) bool { return s.Obs[i].RecvMs >= floor })
	if over := len(s.Obs) - cut - s.Need.Capacity; over > 0 {
		cut += over
	}
	if cut > 0 {
		last := s.Obs[cut-1].RecvMs
		if last+1 > s.RetainedFromMs {
			s.RetainedFromMs = last + 1
		}
		s.Stats.Evicted += uint64(cut)
		s.Obs = append([]feedBookObservation(nil), s.Obs[cut:]...)
	}
	mcut := sort.Search(len(s.Markers), func(i int) bool { return s.Markers[i].AtMs >= floor })
	if mcut > 0 {
		s.Markers = append([]feedBookMarker(nil), s.Markers[mcut:]...)
	}
}

func (s *feedBookState) findEvent(eventMs int64) (feedBookObservation, bool) {
	i := sort.Search(len(s.Obs), func(i int) bool { return s.Obs[i].EventMs >= eventMs })
	if i < len(s.Obs) && s.Obs[i].EventMs == eventMs {
		return s.Obs[i], true
	}
	return feedBookObservation{}, false
}

func truncateLevels(in []feedBookLevel, depth int) []feedBookLevel {
	if len(in) > depth {
		in = in[:depth]
	}
	return append([]feedBookLevel(nil), in...)
}

type feedBookOutcome struct {
	Outcome  string
	Reason   string
	Seq      uint64
	EventMs  int64
	Identity string
}

func (o *marketFeedOwner) ingestBookLocked(st *feedBookState, p feedBookParse, recvMs int64, seq uint64, ev *feedBookEvents) feedBookOutcome {
	st.Stats.Frames++
	out := feedBookOutcome{Seq: seq, EventMs: p.EventMs, Identity: p.Identity}
	reject := func(reason, marker string) feedBookOutcome {
		st.Stats.Rejected++
		st.Stats.LastReject = boundedReason(reason)
		out.Outcome, out.Reason = feedBookOutcomeRejected, boundedReason(reason)
		if st.Covered && marker != "" {
			o.addBookMarkerLocked(st, recvMs, marker, reason, ev)
		}
		return out
	}
	if recvMs < st.HighRecvMs {
		return reject(fmt.Sprintf("clock_regression: receipt %d is before the receipt high-water mark %d", recvMs, st.HighRecvMs), feedBookMarkerClock)
	}
	st.HighRecvMs = recvMs
	if p.Err != nil {
		return reject(p.Err.Error(), feedBookMarkerInvalid)
	}
	if !o.connected || !st.Covered || st.Session != o.obsSession {
		st.Stats.Uncovered++
		out.Outcome, out.Reason = feedBookOutcomeUncovered, "no acknowledged l2Book subscription in this socket session"
		return out
	}
	if p.EventMs > recvMs+feedBookMaxFutureSkewMs {
		return reject(fmt.Sprintf("future_event: event %d is more than %dms after receipt %d", p.EventMs, feedBookMaxFutureSkewMs, recvMs), feedBookMarkerInvalid)
	}
	if recvMs-p.EventMs > feedBookMaxLagMs {
		return reject(fmt.Sprintf("stale_receipt: receipt %d is more than %dms after event %d", recvMs, feedBookMaxLagMs, p.EventMs), feedBookMarkerInvalid)
	}
	if prior, ok := st.findEvent(p.EventMs); ok {
		if prior.Identity == p.Identity {
			st.Stats.Duplicates++
			out.Outcome = feedBookOutcomeDuplicate
			return out
		}
		st.Stats.Conflicts++
		out.Outcome, out.Reason = feedBookOutcomeConflict, "same source event time with different content"
		o.addBookMarkerLocked(st, recvMs, feedBookMarkerConflict, out.Reason, ev)
		return out
	}
	if p.EventMs <= st.HighEventMs {
		st.Stats.OutOfOrder++
		out.Outcome = feedBookOutcomeOutOfOrder
		out.Reason = fmt.Sprintf("event %d is not after the event high-water mark %d", p.EventMs, st.HighEventMs)
		o.addBookMarkerLocked(st, recvMs, feedBookMarkerOutOfOrder, out.Reason, ev)
		return out
	}
	if st.LastAcceptMs > 0 && !st.SilenceMarked && recvMs-st.LastAcceptMs > feedBookMaxGapMs {
		o.addBookMarkerLocked(st, recvMs, feedBookMarkerSilence, fmt.Sprintf("receipt gap %dms over %dms", recvMs-st.LastAcceptMs, feedBookMaxGapMs), ev)
	} else if st.HighEventMs > 0 && !st.SilenceMarked && p.EventMs-st.HighEventMs > feedBookMaxGapMs {
		o.addBookMarkerLocked(st, recvMs, feedBookMarkerSilence, fmt.Sprintf("event gap %dms over %dms", p.EventMs-st.HighEventMs, feedBookMaxGapMs), ev)
	}
	obs := feedBookObservation{Seq: seq, EventMs: p.EventMs, RecvMs: recvMs, Session: o.obsSession, Identity: p.Identity,
		BidDepth: len(p.Bids), AskDepth: len(p.Asks), Bids: truncateLevels(p.Bids, st.Need.Depth), Asks: truncateLevels(p.Asks, st.Need.Depth)}
	st.Obs = append(st.Obs, obs)
	st.Stats.Accepted++
	st.HighEventMs = p.EventMs
	st.LastAcceptMs = recvMs
	st.SilenceMarked = false
	out.Outcome = feedBookOutcomeAccepted
	st.trim(recvMs)
	return out
}

func (o *marketFeedOwner) emitBookEventsLocked(ev feedBookEvents) {
	if o.recorder == nil {
		return
	}
	for _, e := range ev {
		o.recorder.offer(observationRecord{Kind: e.Kind, RecvMs: e.RecvMs, Session: o.obsSession, Extra: e.Fields})
	}
}

func sortedBookKeys(m map[feedObservationKey]*feedBookState) []feedObservationKey {
	keys := make([]feedObservationKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	return keys
}

func (o *marketFeedOwner) BookCoins() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make([]string, 0, len(o.books))
	for k := range o.books {
		out = append(out, k.Coin)
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) IngestBookFrame(data json.RawMessage, recvAt time.Time, raw []byte) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.books) == 0 {
		return
	}
	seq := o.nextBookSeq()
	var ev feedBookEvents
	record := map[string]any{"recv_ms": recvMs, "kind": feedObservationL2Book, "seq": seq, "bytes": len(raw)}
	if len(raw) <= feedBookMaxFrameBytes {
		record["raw"] = string(raw)
	} else {
		sum := sha256.Sum256(raw)
		record["raw_sha256"] = hex.EncodeToString(sum[:])
	}
	var p feedBookParse
	if len(raw) > feedBookMaxFrameBytes {
		p = feedBookParse{Err: fmt.Errorf("malformed: the frame is %d bytes, over the %d-byte bound", len(raw), feedBookMaxFrameBytes)}
	} else {
		p = parseBookFrame(l2BookKeyFor, data)
	}
	record["coin"] = p.Coin
	st := o.books[l2BookKeyFor(p.Coin)]
	switch {
	case p.Coin == "":
		reason := boundedReason(p.Err.Error())
		record["outcome"], record["reason"] = feedBookOutcomeRejected, reason
		o.markBooksMalformedLocked(recvMs, reason, &ev)
	case st == nil:
		record["outcome"] = feedBookOutcomeUntracked
	default:
		out := o.ingestBookLocked(st, p, recvMs, seq, &ev)
		record["outcome"], record["reason"], record["event_ms"], record["identity"] = out.Outcome, out.Reason, out.EventMs, out.Identity
	}
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "book", RecvMs: recvMs, Session: o.obsSession, Extra: record})
	}
	o.emitBookEventsLocked(ev)
}

func (o *marketFeedOwner) markBooksMalformedLocked(atMs int64, reason string, ev *feedBookEvents) {
	for _, k := range sortedBookKeys(o.books) {
		st := o.books[k]
		st.Stats.Rejected++
		st.Stats.LastReject = reason
		if st.Covered {
			o.addBookMarkerLocked(st, atMs, feedBookMarkerInvalid, reason, ev)
		}
	}
}

func (o *marketFeedOwner) IngestMalformedBookEnvelope(recvAt time.Time, raw []byte) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if len(o.books) == 0 {
		return
	}
	seq := o.nextBookSeq()
	reason := "malformed: the socket message is not a JSON envelope"
	record := map[string]any{"recv_ms": recvMs, "kind": feedObservationL2Book, "seq": seq, "coin": "", "bytes": len(raw),
		"outcome": feedBookOutcomeRejected, "reason": reason}
	if len(raw) <= feedBookMaxFrameBytes {
		record["raw"] = string(raw)
	} else {
		sum := sha256.Sum256(raw)
		record["raw_sha256"] = hex.EncodeToString(sum[:])
	}
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "book", RecvMs: recvMs, Session: o.obsSession, Extra: record})
	}
	var ev feedBookEvents
	o.markBooksMalformedLocked(recvMs, reason, &ev)
	o.emitBookEventsLocked(ev)
}

type feedBookSubscriptionAck struct {
	Method       string                     `json:"method"`
	Subscription map[string]json.RawMessage `json:"subscription"`
}

func bookAckContractError(sub map[string]json.RawMessage) string {
	for _, name := range []string{"nSigFigs", "mantissa"} {
		if raw, ok := sub[name]; ok && strings.TrimSpace(string(raw)) != "null" {
			return fmt.Sprintf("the acknowledgement echoes %s=%s; the request omits it (native precision)", name, string(raw))
		}
	}
	raw, ok := sub["fast"]
	if !ok {
		return "the acknowledgement does not echo fast=false"
	}
	if strings.TrimSpace(string(raw)) != "false" {
		return fmt.Sprintf("the acknowledgement echoes fast=%s; the request is fast=false", string(raw))
	}
	return ""
}

func (o *marketFeedOwner) IngestBookSubscription(coin string, recvAt time.Time, raw []byte, sub map[string]json.RawMessage) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.books[l2BookKeyFor(coin)]
	if st == nil {
		return
	}
	why := bookAckContractError(sub)
	if !o.connected && why == "" {
		why = "the socket is not connected"
	}
	if o.recorder != nil {
		o.recorder.offer(observationRecord{Kind: "book_sub", RecvMs: recvMs, Session: o.obsSession, Extra: map[string]any{
			"coin": coin, "kind": feedObservationL2Book, "raw": string(raw), "recv_ms": recvMs, "accepted": why == "", "reason": why}})
	}
	var ev feedBookEvents
	if why != "" {
		o.logf("[feed] %s: refused an l2Book acknowledgement: %s", st.Key.PayloadID(), why)
		if st.Covered {
			o.addBookMarkerLocked(st, recvMs, feedBookMarkerSubFailed, why, &ev)
		}
		st.Covered = false
		o.emitBookEventsLocked(ev)
		return
	}
	if st.Covered && st.Session == o.obsSession {
		return
	}
	st.Covered, st.Session = true, o.obsSession
	st.LastAcceptMs, st.SilenceMarked = recvMs, false
	o.addBookMarkerLocked(st, recvMs, feedBookMarkerSubscribed, "", &ev)
	o.emitBookEventsLocked(ev)
}

func (o *marketFeedOwner) markBooksDisconnectedLocked(atMs int64) {
	var ev feedBookEvents
	for _, k := range sortedBookKeys(o.books) {
		st := o.books[k]
		if !st.Covered {
			continue
		}
		o.addBookMarkerLocked(st, atMs, feedBookMarkerDisconnected, "", &ev)
		st.Covered = false
	}
	o.emitBookEventsLocked(ev)
}

func (o *marketFeedOwner) tickBooksLocked(nowMs int64) {
	var ev feedBookEvents
	for _, k := range sortedBookKeys(o.books) {
		st := o.books[k]
		if !st.Covered || st.SilenceMarked || st.LastAcceptMs == 0 || nowMs-st.LastAcceptMs <= feedBookMaxGapMs {
			continue
		}
		o.addBookMarkerLocked(st, nowMs, feedBookMarkerSilence, fmt.Sprintf("no accepted book for %dms, over %dms", nowMs-st.LastAcceptMs, feedBookMaxGapMs), &ev)
		st.SilenceMarked = true
	}
	o.emitBookEventsLocked(ev)
}

func (o *marketFeedOwner) TickBooks(now time.Time) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.tickBooksLocked(now.UTC().UnixMilli())
}

func (o *marketFeedOwner) applyBookNeedsLocked(needs map[feedObservationKey]feedBookNeed) {
	nowMs := o.now().UnixMilli()
	var ev feedBookEvents
	for _, k := range sortedBookNeedKeys(needs) {
		need := needs[k]
		st := o.books[k]
		if st == nil {
			st = &feedBookState{Key: k, Need: need, RetainedFromMs: nowMs, DepthFromMs: nowMs, StartedMs: nowMs}
			o.books[k] = st
			o.addBookMarkerLocked(st, nowMs, feedBookMarkerEpoch, "owner "+o.bookInstance, &ev)
			if o.recorder != nil {
				ev = append(ev, feedBookEvent{Kind: "book_need", RecvMs: nowMs, Fields: map[string]any{"coin": k.Coin, "kind": feedObservationL2Book,
					"window_ms": need.WindowMs, "capacity": need.Capacity, "depth": need.Depth, "recv_ms": nowMs, "instance": o.bookInstance}})
			}
			continue
		}
		if !st.Need.sameSemantics(need) {
			continue
		}
		if need.Depth > st.Need.Depth {
			st.DepthFromMs = nowMs
			st.Stats.Evicted += uint64(len(st.Obs))
			st.Obs = nil
			o.addBookMarkerLocked(st, nowMs, feedBookMarkerDepthRaised, fmt.Sprintf("depth raised from %d to %d levels", st.Need.Depth, need.Depth), &ev)
		}
		st.Need = need
		if o.recorder != nil {
			ev = append(ev, feedBookEvent{Kind: "book_need", RecvMs: nowMs, Fields: map[string]any{"coin": k.Coin, "kind": feedObservationL2Book,
				"window_ms": st.Need.WindowMs, "capacity": st.Need.Capacity, "depth": st.Need.Depth, "recv_ms": nowMs, "instance": o.bookInstance}})
		}
		st.trim(nowMs)
	}
	o.emitBookEventsLocked(ev)
}

func (o *marketFeedOwner) dropUnneededBooksLocked(needs map[feedObservationKey]feedBookNeed) {
	nowMs := o.now().UnixMilli()
	for _, k := range sortedBookKeys(o.books) {
		if _, ok := needs[k]; ok {
			continue
		}
		delete(o.books, k)
		if o.recorder != nil {
			o.recorder.offer(observationRecord{Kind: "book_release", RecvMs: nowMs, Session: o.obsSession, Extra: map[string]any{
				"coin": k.Coin, "kind": feedObservationL2Book, "recv_ms": nowMs}})
		}
	}
}

func (o *marketFeedOwner) trackBooks(coins []string, need feedBookNeed) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	needs := make(map[feedObservationKey]feedBookNeed, len(coins))
	for _, c := range coins {
		needs[l2BookKeyFor(c)] = need
	}
	o.applyBookNeedsLocked(needs)
	o.subVersion++
}

type feedBookReadiness struct {
	Key          feedObservationKey
	Status       string
	Detail       string
	Ready        bool
	Observations int
	Markers      int
	CoveredMs    int64
	WindowMs     int64
	LastRecvMs   int64
	Stats        feedBookStats
}

func bookCoverageFrom(s *feedBookState) int64 {
	from := s.RetainedFromMs
	if s.DepthFromMs > from {
		from = s.DepthFromMs
	}
	return from
}

func bookReadiness(s *feedBookState, nowMs int64) feedBookReadiness {
	out := feedBookReadiness{Key: s.Key, Observations: len(s.Obs), Markers: len(s.Markers), WindowMs: s.Need.WindowMs, Stats: s.Stats}
	if n := len(s.Obs); n > 0 {
		out.LastRecvMs = s.Obs[n-1].RecvMs
	}
	if !s.Covered {
		out.Status, out.Detail = feedBookStatusNoCoverage, "no acknowledged l2Book subscription in this socket session"
		return out
	}
	from := bookCoverageFrom(s)
	if n := len(s.Markers); n > 0 && s.Markers[n-1].AtMs > from {
		from = s.Markers[n-1].AtMs
	}
	out.CoveredMs = nowMs - from
	if out.LastRecvMs == 0 || nowMs-out.LastRecvMs > feedBookMaxGapMs {
		out.Status, out.Detail = feedObservationStatusStale, fmt.Sprintf("no accepted book within %dms", feedBookMaxGapMs)
		return out
	}
	if out.CoveredMs < s.Need.WindowMs {
		out.Status, out.Detail = feedObservationStatusWarming, fmt.Sprintf("%dms of book history since the last marker, %dms required", out.CoveredMs, s.Need.WindowMs)
		return out
	}
	out.Status, out.Ready = feedObservationStatusReady, true
	return out
}

func (o *marketFeedOwner) BookReadiness() []feedBookReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	out := make([]feedBookReadiness, 0, len(o.books))
	for _, k := range sortedBookKeys(o.books) {
		out = append(out, bookReadiness(o.books[k], nowMs))
	}
	return out
}

type marketSnapshotBook struct {
	Key            feedObservationKey
	Need           feedBookNeed
	Instance       string
	CutoffMs       int64
	CoverageFromMs int64
	Obs            []feedBookObservation
	Markers        []feedBookMarker
	Readiness      feedBookReadiness
}

func copyBookObservation(b feedBookObservation, depth int) feedBookObservation {
	b.Bids = truncateLevels(b.Bids, depth)
	b.Asks = truncateLevels(b.Asks, depth)
	return b
}

func freezeBook(st *feedBookState, need feedBookNeed, instance string, cutoffMs, nowMs int64) *marketSnapshotBook {
	start := cutoffMs - need.WindowMs
	out := &marketSnapshotBook{Key: st.Key, Need: need, Instance: instance, CutoffMs: cutoffMs,
		Obs: []feedBookObservation{}, Markers: []feedBookMarker{}, Readiness: bookReadiness(st, nowMs)}
	out.CoverageFromMs = bookCoverageFrom(st)
	for _, b := range st.Obs {
		if b.RecvMs < start || b.RecvMs > cutoffMs {
			continue
		}
		out.Obs = append(out.Obs, copyBookObservation(b, need.Depth))
	}
	for _, m := range st.Markers {
		if m.AtMs < start || m.AtMs > cutoffMs {
			continue
		}
		out.Markers = append(out.Markers, m)
	}
	return out
}

func (s *marketSnapshotBook) prefixComplete(windowMs int64) bool {
	return s.CoverageFromMs <= s.CutoffMs-windowMs
}

type marketBookRecord struct {
	Seq      uint64          `json:"seq"`
	EventMs  int64           `json:"event_ms"`
	RecvMs   int64           `json:"recv_ms"`
	Session  uint64          `json:"session"`
	Identity string          `json:"identity"`
	BidDepth int             `json:"bid_depth"`
	AskDepth int             `json:"ask_depth"`
	Bids     []feedBookLevel `json:"bids"`
	Asks     []feedBookLevel `json:"asks"`
}

type marketBookMarker struct {
	Seq     uint64 `json:"seq"`
	AtMs    int64  `json:"at_ms"`
	Reason  string `json:"reason"`
	Session uint64 `json:"session"`
	Detail  string `json:"detail,omitempty"`
}

type marketL2BookPayload struct {
	Kind              string             `json:"kind"`
	Coin              string             `json:"coin"`
	Source            string             `json:"source"`
	Contract          string             `json:"contract"`
	TimeSemantics     string             `json:"time_semantics"`
	IdentitySemantics string             `json:"identity_semantics"`
	MaxFutureSkewMs   int64              `json:"max_future_skew_ms"`
	MaxLagMs          int64              `json:"max_lag_ms"`
	MaxGapMs          int64              `json:"max_gap_ms"`
	WindowMs          int64              `json:"window_ms"`
	Capacity          int                `json:"capacity"`
	Depth             int                `json:"depth"`
	CutoffMs          int64              `json:"cutoff_ms"`
	RetainedFromMs    int64              `json:"retained_from_ms"`
	PrefixComplete    bool               `json:"prefix_complete"`
	Instance          string             `json:"owner_instance"`
	SnapshotID        string             `json:"snapshot_id"`
	Available         bool               `json:"available"`
	Reason            string             `json:"reason,omitempty"`
	FeedStatus        string             `json:"feed_status,omitempty"`
	Observations      []marketBookRecord `json:"observations"`
	Markers           []marketBookMarker `json:"markers"`
}

func newMarketL2BookPayload(coin string, need feedBookNeed) marketL2BookPayload {
	return marketL2BookPayload{Kind: feedObservationL2Book, Coin: coin, Source: need.Source, Contract: need.Contract,
		TimeSemantics: need.TimeSemantics, IdentitySemantics: need.IdentitySemantics, MaxFutureSkewMs: feedBookMaxFutureSkewMs,
		MaxLagMs: feedBookMaxLagMs, MaxGapMs: feedBookMaxGapMs, WindowMs: need.WindowMs, Capacity: need.Capacity, Depth: need.Depth,
		Observations: []marketBookRecord{}, Markers: []marketBookMarker{}}
}

func (s *marketSnapshot) bookEntry(key feedObservationKey, need feedBookNeed) (*marketSnapshotBook, string) {
	if s == nil {
		return nil, "no sealed market snapshot"
	}
	entry, ok := s.books[key]
	if !ok || entry == nil {
		return nil, fmt.Sprintf("%s is not in the sealed snapshot", key.PayloadID())
	}
	switch {
	case !entry.Need.sameSemantics(need):
		return entry, fmt.Sprintf("%s seal semantics %q do not match the need %q", key.PayloadID(), entry.Need.Contract, need.Contract)
	case entry.Need.WindowMs < need.WindowMs:
		return entry, fmt.Sprintf("%s seal window %dms is below the %dms this need requires", key.PayloadID(), entry.Need.WindowMs, need.WindowMs)
	case entry.Need.Capacity < need.Capacity:
		return entry, fmt.Sprintf("%s seal capacity %d is below the %d this need requires", key.PayloadID(), entry.Need.Capacity, need.Capacity)
	case entry.Need.Depth < need.Depth:
		return entry, fmt.Sprintf("%s seal depth %d is below the %d levels this need requires", key.PayloadID(), entry.Need.Depth, need.Depth)
	}
	return entry, ""
}

func (s *marketSnapshot) l2BookPayload(key feedObservationKey, need feedBookNeed) marketL2BookPayload {
	out := newMarketL2BookPayload(key.Coin, need)
	if s != nil {
		out.SnapshotID = s.EvaluationID
		out.CutoffMs = s.observationCutoff().UnixMilli()
	}
	entry, why := s.bookEntry(key, need)
	if entry != nil {
		out.CutoffMs, out.FeedStatus, out.Instance = entry.CutoffMs, entry.Readiness.Status, entry.Instance
	}
	if why != "" {
		out.Reason = why
		return out
	}
	start := entry.CutoffMs - need.WindowMs
	out.RetainedFromMs = entry.CoverageFromMs
	out.PrefixComplete = entry.prefixComplete(need.WindowMs)
	count := 0
	for _, b := range entry.Obs {
		if b.RecvMs < start {
			continue
		}
		count++
	}
	skip := 0
	if count > need.Capacity {
		skip = count - need.Capacity
		out.PrefixComplete = false
	}
	for _, b := range entry.Obs {
		if b.RecvMs < start {
			continue
		}
		if skip > 0 {
			skip--
			if b.RecvMs+1 > out.RetainedFromMs {
				out.RetainedFromMs = b.RecvMs + 1
			}
			continue
		}
		c := copyBookObservation(b, need.Depth)
		out.Observations = append(out.Observations, marketBookRecord{Seq: c.Seq, EventMs: c.EventMs, RecvMs: c.RecvMs, Session: c.Session,
			Identity: c.Identity, BidDepth: c.BidDepth, AskDepth: c.AskDepth, Bids: c.Bids, Asks: c.Asks})
	}
	for _, m := range entry.Markers {
		if m.AtMs < start {
			continue
		}
		out.Markers = append(out.Markers, marketBookMarker{Seq: m.Seq, AtMs: m.AtMs, Reason: m.Reason, Session: m.Session, Detail: m.Detail})
	}
	out.Available = true
	return out
}

func refusedL2BookSection(coin string, need feedBookNeed, cutoffMs int64, snapshotID, reason string) marketL2BookPayload {
	out := newMarketL2BookPayload(coin, need)
	out.CutoffMs, out.SnapshotID, out.Reason = cutoffMs, snapshotID, boundedReason(reason)
	return out
}

func attachL2BookPayloads(payload *marketPayload, s *marketSnapshot, needs map[feedObservationKey]feedBookNeed, reserveBytes int) {
	if payload == nil || len(needs) == 0 {
		return
	}
	base, err := json.Marshal(payload)
	if err != nil {
		return
	}
	budget := marketPayloadMaxBytes - len(base) - reserveBytes
	if budget > feedBookEnvelopeBudget {
		budget = feedBookEnvelopeBudget
	}
	payload.L2Books = make(map[string]marketL2BookPayload, len(needs))
	used := 0
	for _, k := range sortedBookNeedKeys(needs) {
		section := s.l2BookPayload(k, needs[k])
		blob, _ := json.Marshal(section)
		if used+len(blob)+len(k.PayloadID())+8 > budget {
			refused := refusedL2BookSection(k.Coin, needs[k], section.CutoffMs, section.SnapshotID,
				fmt.Sprintf("%s: %s is %d bytes and the remaining optional book budget is %d bytes", feedBookRefusedCapacity, k.PayloadID(), len(blob), budget-used))
			rblob, _ := json.Marshal(refused)
			if used+len(rblob)+len(k.PayloadID())+8 > budget {
				payload.L2Books = nil
				payload.L2BooksRefused = feedBookRefusedCapacity + ": not even the book refusal metadata fits this envelope"
				return
			}
			used += len(rblob) + len(k.PayloadID()) + 8
			payload.L2Books[k.PayloadID()] = refused
			continue
		}
		used += len(blob) + len(k.PayloadID()) + 8
		payload.L2Books[k.PayloadID()] = section
	}
}

func refuseAllL2BookSections(payload *marketPayload, reason string) bool {
	if payload == nil || (len(payload.L2Books) == 0 && payload.L2BooksRefused == "") {
		return false
	}
	changed := false
	for id, b := range payload.L2Books {
		if !b.Available && len(b.Observations) == 0 && len(b.Markers) == 0 {
			continue
		}
		need := contractBookNeed(b.WindowMs, b.Capacity, b.Depth)
		payload.L2Books[id] = refusedL2BookSection(b.Coin, need, b.CutoffMs, b.SnapshotID, reason)
		changed = true
	}
	return changed
}

func (s *marketSnapshot) bookHold(entry feedStrategyRequirement) (bool, string) {
	if entry.Book == nil {
		return false, ""
	}
	e, why := s.bookEntry(l2BookKeyFor(entry.Coin), *entry.Book)
	if why != "" {
		return true, why
	}
	if !e.prefixComplete(entry.Book.WindowMs) {
		return true, fmt.Sprintf("%s history is complete only from %d, after the window start %d", e.Key.PayloadID(), e.CoverageFromMs, e.CutoffMs-entry.Book.WindowMs)
	}
	return false, ""
}

type feedSealBookRecord struct {
	Seq      uint64          `json:"q"`
	EventMs  int64           `json:"e"`
	RecvMs   int64           `json:"r"`
	Session  uint64          `json:"s"`
	Identity string          `json:"h"`
	BidDepth int             `json:"bd"`
	AskDepth int             `json:"ad"`
	Bids     []feedBookLevel `json:"b"`
	Asks     []feedBookLevel `json:"a"`
}

type feedSealBookMarker struct {
	Seq     uint64 `json:"q"`
	AtMs    int64  `json:"t"`
	Reason  string `json:"why"`
	Session uint64 `json:"s"`
	Detail  string `json:"d"`
}

type feedSealBookReadiness struct {
	Status       string `json:"status"`
	Detail       string `json:"detail"`
	Ready        bool   `json:"ready"`
	Observations int    `json:"observations"`
	Markers      int    `json:"markers"`
	CoveredMs    int64  `json:"covered_ms"`
	LastRecvMs   int64  `json:"last_recv_ms"`
	Frames       uint64 `json:"frames"`
	Accepted     uint64 `json:"accepted"`
	Duplicates   uint64 `json:"duplicates"`
	Conflicts    uint64 `json:"conflicts"`
	OutOfOrder   uint64 `json:"out_of_order"`
	Rejected     uint64 `json:"rejected"`
	Uncovered    uint64 `json:"uncovered"`
	MarkerCount  uint64 `json:"marker_count"`
	Evicted      uint64 `json:"evicted"`
	LastReject   string `json:"last_reject"`
}

type feedSealL2Book struct {
	Host              string                `json:"host"`
	Namespace         string                `json:"namespace"`
	Coin              string                `json:"coin"`
	Kind              string                `json:"kind"`
	Source            string                `json:"source"`
	Contract          string                `json:"contract"`
	TimeSemantics     string                `json:"time_semantics"`
	IdentitySemantics string                `json:"identity_semantics"`
	WindowMs          int64                 `json:"window_ms"`
	Capacity          int                   `json:"capacity"`
	Depth             int                   `json:"depth"`
	Instance          string                `json:"owner_instance"`
	CutoffMs          int64                 `json:"cutoff_ms"`
	CoverageFromMs    int64                 `json:"coverage_from_ms"`
	Readiness         feedSealBookReadiness `json:"readiness"`
	Observations      []feedSealBookRecord  `json:"observations"`
	Markers           []feedSealBookMarker  `json:"markers"`
}

func sealL2Books(books map[feedObservationKey]*marketSnapshotBook) []feedSealL2Book {
	keys := make([]feedObservationKey, 0, len(books))
	for k, v := range books {
		if v != nil {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sortFeedObservationKeys(keys)
	out := make([]feedSealL2Book, 0, len(keys))
	for _, k := range keys {
		b := books[k]
		r := b.Readiness
		sb := feedSealL2Book{
			Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, Kind: k.Kind, Source: b.Need.Source, Contract: b.Need.Contract,
			TimeSemantics: b.Need.TimeSemantics, IdentitySemantics: b.Need.IdentitySemantics, WindowMs: b.Need.WindowMs,
			Capacity: b.Need.Capacity, Depth: b.Need.Depth, Instance: b.Instance, CutoffMs: b.CutoffMs, CoverageFromMs: b.CoverageFromMs,
			Readiness: feedSealBookReadiness{Status: r.Status, Detail: boundedReason(r.Detail), Ready: r.Ready, Observations: r.Observations,
				Markers: r.Markers, CoveredMs: r.CoveredMs, LastRecvMs: r.LastRecvMs, Frames: r.Stats.Frames, Accepted: r.Stats.Accepted,
				Duplicates: r.Stats.Duplicates, Conflicts: r.Stats.Conflicts, OutOfOrder: r.Stats.OutOfOrder, Rejected: r.Stats.Rejected,
				Uncovered: r.Stats.Uncovered, MarkerCount: r.Stats.Markers, Evicted: r.Stats.Evicted, LastReject: r.Stats.LastReject},
			Observations: make([]feedSealBookRecord, 0, len(b.Obs)),
			Markers:      make([]feedSealBookMarker, 0, len(b.Markers)),
		}
		for _, o := range b.Obs {
			sb.Observations = append(sb.Observations, feedSealBookRecord{Seq: o.Seq, EventMs: o.EventMs, RecvMs: o.RecvMs, Session: o.Session,
				Identity: o.Identity, BidDepth: o.BidDepth, AskDepth: o.AskDepth, Bids: append([]feedBookLevel{}, o.Bids...), Asks: append([]feedBookLevel{}, o.Asks...)})
		}
		for _, m := range b.Markers {
			sb.Markers = append(sb.Markers, feedSealBookMarker{Seq: m.Seq, AtMs: m.AtMs, Reason: m.Reason, Session: m.Session, Detail: m.Detail})
		}
		out = append(out, sb)
	}
	return out
}

func validBookText(s string, max int) bool {
	return len(s) <= max && boundedReason(s) == s
}

func validateFeedSealL2Books(books []feedSealL2Book, keyMs int64) error {
	if len(books) > feedBookMaxKeys {
		return fmt.Errorf("seal carries %d l2 books, over the %d-key bound", len(books), feedBookMaxKeys)
	}
	var prev *feedObservationKey
	for i := range books {
		b := books[i]
		key := feedObservationKey{Host: b.Host, Namespace: b.Namespace, Coin: b.Coin, Kind: b.Kind}
		if prev != nil && !feedObservationKeyLess(*prev, key) {
			return fmt.Errorf("seal l2 books are not strictly ordered at %s", key)
		}
		prev = &key
		if b.Kind != feedObservationL2Book || b.Coin == "" || len(b.Host) > feedBookMaxHostLen || len(b.Namespace) > feedBookMaxNamespace ||
			len(b.Coin) > feedBookMaxCoinLen || !validBookText(b.Instance, feedBookMaxInstanceLen) || b.Instance == "" {
			return fmt.Errorf("seal l2 book %s has a malformed key or owner instance", key)
		}
		need := feedBookNeed{Source: b.Source, Contract: b.Contract, TimeSemantics: b.TimeSemantics, IdentitySemantics: b.IdentitySemantics,
			WindowMs: b.WindowMs, Capacity: b.Capacity, Depth: b.Depth}
		if err := need.validate(); err != nil {
			return fmt.Errorf("seal l2 book %s: %v", key, err)
		}
		if b.CutoffMs <= 0 || b.CutoffMs != keyMs || b.CoverageFromMs <= 0 {
			return fmt.Errorf("seal l2 book %s has cutoff %d for key time %d, coverage %d", key, b.CutoffMs, keyMs, b.CoverageFromMs)
		}
		if len(b.Observations) > feedBookMaxObservations || len(b.Markers) > feedBookMaxMarkers {
			return fmt.Errorf("seal l2 book %s carries %d observations and %d markers, over the bounds", key, len(b.Observations), len(b.Markers))
		}
		start := b.CutoffMs - b.WindowMs
		seqs := make(map[uint64]bool, len(b.Observations)+len(b.Markers))
		var last *feedSealBookRecord
		for j := range b.Observations {
			r := b.Observations[j]
			if r.Seq == 0 || seqs[r.Seq] || r.EventMs <= 0 || r.RecvMs < start || r.RecvMs > b.CutoffMs ||
				r.EventMs > r.RecvMs+feedBookMaxFutureSkewMs || r.RecvMs-r.EventMs > feedBookMaxLagMs {
				return fmt.Errorf("seal l2 book %s observation %d has malformed sequence or times", key, j)
			}
			if last != nil && (r.Seq <= last.Seq || r.EventMs <= last.EventMs || r.RecvMs < last.RecvMs) {
				return fmt.Errorf("seal l2 book %s observations are out of sequence, event or receipt order at %d", key, j)
			}
			if r.BidDepth < len(r.Bids) || r.AskDepth < len(r.Asks) || r.BidDepth > feedBookMaxLevels || r.AskDepth > feedBookMaxLevels ||
				len(r.Bids) != minInt(r.BidDepth, b.Depth) || len(r.Asks) != minInt(r.AskDepth, b.Depth) {
				return fmt.Errorf("seal l2 book %s observation %d declares depths %d/%d with %d/%d levels at depth %d", key, j, r.BidDepth, r.AskDepth, len(r.Bids), len(r.Asks), b.Depth)
			}
			if err := validateBookSides(r.Bids, r.Asks); err != nil {
				return fmt.Errorf("seal l2 book %s observation %d: %v", key, j, err)
			}
			if hb, err := hex.DecodeString(r.Identity); err != nil || len(hb) != sha256.Size || strings.ToLower(r.Identity) != r.Identity {
				return fmt.Errorf("seal l2 book %s observation %d identity is not 64 lowercase hex characters", key, j)
			}
			if r.BidDepth == len(r.Bids) && r.AskDepth == len(r.Asks) && bookIdentity(key, r.EventMs, r.Bids, r.Asks) != r.Identity {
				return fmt.Errorf("seal l2 book %s observation %d identity does not match its full levels", key, j)
			}
			seqs[r.Seq] = true
			last = &b.Observations[j]
		}
		var lastM *feedSealBookMarker
		for j := range b.Markers {
			m := b.Markers[j]
			if m.Seq == 0 || seqs[m.Seq] || m.AtMs < start || m.AtMs > b.CutoffMs || !feedBookMarkerReasons[m.Reason] || !validBookText(m.Detail, feedBookMaxReasonLen) {
				return fmt.Errorf("seal l2 book %s marker %d is malformed", key, j)
			}
			if lastM != nil && (m.Seq <= lastM.Seq || m.AtMs < lastM.AtMs) {
				return fmt.Errorf("seal l2 book %s markers are out of order at %d", key, j)
			}
			seqs[m.Seq] = true
			lastM = &b.Markers[j]
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func snapshotL2Books(books []feedSealL2Book) map[feedObservationKey]*marketSnapshotBook {
	out := make(map[feedObservationKey]*marketSnapshotBook, len(books))
	for _, b := range books {
		key := feedObservationKey{Host: b.Host, Namespace: b.Namespace, Coin: b.Coin, Kind: b.Kind}
		r := b.Readiness
		entry := &marketSnapshotBook{Key: key, Instance: b.Instance, CutoffMs: b.CutoffMs, CoverageFromMs: b.CoverageFromMs,
			Need: feedBookNeed{Source: b.Source, Contract: b.Contract, TimeSemantics: b.TimeSemantics, IdentitySemantics: b.IdentitySemantics,
				WindowMs: b.WindowMs, Capacity: b.Capacity, Depth: b.Depth},
			Obs: make([]feedBookObservation, 0, len(b.Observations)), Markers: make([]feedBookMarker, 0, len(b.Markers)),
			Readiness: feedBookReadiness{Key: key, Status: r.Status, Detail: r.Detail, Ready: r.Ready, Observations: r.Observations,
				Markers: r.Markers, CoveredMs: r.CoveredMs, WindowMs: b.WindowMs, LastRecvMs: r.LastRecvMs,
				Stats: feedBookStats{Frames: r.Frames, Accepted: r.Accepted, Duplicates: r.Duplicates, Conflicts: r.Conflicts, OutOfOrder: r.OutOfOrder,
					Rejected: r.Rejected, Uncovered: r.Uncovered, Markers: r.MarkerCount, Evicted: r.Evicted, LastReject: r.LastReject}},
		}
		for _, o := range b.Observations {
			entry.Obs = append(entry.Obs, feedBookObservation{Seq: o.Seq, EventMs: o.EventMs, RecvMs: o.RecvMs, Session: o.Session, Identity: o.Identity,
				BidDepth: o.BidDepth, AskDepth: o.AskDepth, Bids: append([]feedBookLevel(nil), o.Bids...), Asks: append([]feedBookLevel(nil), o.Asks...)})
		}
		for _, m := range b.Markers {
			entry.Markers = append(entry.Markers, feedBookMarker{Seq: m.Seq, AtMs: m.AtMs, Reason: m.Reason, Session: m.Session, Detail: m.Detail})
		}
		out[key] = entry
	}
	return out
}

type feedDescribeBook struct {
	Host              string `json:"host"`
	Namespace         string `json:"namespace"`
	Coin              string `json:"coin"`
	Kind              string `json:"kind"`
	Source            string `json:"source"`
	Contract          string `json:"contract"`
	TimeSemantics     string `json:"time_semantics"`
	IdentitySemantics string `json:"identity_semantics"`
	WindowMs          int64  `json:"window_ms"`
	Capacity          int    `json:"capacity"`
	Depth             int    `json:"depth"`
	MaxFutureSkewMs   int64  `json:"max_future_skew_ms"`
	MaxLagMs          int64  `json:"max_lag_ms"`
	MaxGapMs          int64  `json:"max_gap_ms"`
}

func describeBooks(books map[feedObservationKey]feedBookNeed) []feedDescribeBook {
	if len(books) == 0 {
		return nil
	}
	out := make([]feedDescribeBook, 0, len(books))
	for _, k := range sortedBookNeedKeys(books) {
		n := books[k]
		out = append(out, feedDescribeBook{Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, Kind: k.Kind, Source: n.Source, Contract: n.Contract,
			TimeSemantics: n.TimeSemantics, IdentitySemantics: n.IdentitySemantics, WindowMs: n.WindowMs, Capacity: n.Capacity, Depth: n.Depth,
			MaxFutureSkewMs: feedBookMaxFutureSkewMs, MaxLagMs: feedBookMaxLagMs, MaxGapMs: feedBookMaxGapMs})
	}
	return out
}

func describedBookGap(need feedBookNeed, got feedDescribeBook) string {
	switch {
	case got.Source != need.Source || got.Contract != need.Contract:
		return fmt.Sprintf("source %q contract %q is not %q %q", got.Source, got.Contract, need.Source, need.Contract)
	case got.TimeSemantics != need.TimeSemantics || got.IdentitySemantics != need.IdentitySemantics:
		return fmt.Sprintf("time semantics %q or identity semantics %q differ", got.TimeSemantics, got.IdentitySemantics)
	case got.MaxFutureSkewMs != feedBookMaxFutureSkewMs || got.MaxLagMs != feedBookMaxLagMs || got.MaxGapMs != feedBookMaxGapMs:
		return fmt.Sprintf("time bounds %d/%d/%d ms differ from %d/%d/%d ms", got.MaxFutureSkewMs, got.MaxLagMs, got.MaxGapMs, feedBookMaxFutureSkewMs, feedBookMaxLagMs, feedBookMaxGapMs)
	case got.WindowMs < need.WindowMs:
		return fmt.Sprintf("window %dms < %dms", got.WindowMs, need.WindowMs)
	case got.Capacity < need.Capacity:
		return fmt.Sprintf("capacity %d < %d observations", got.Capacity, need.Capacity)
	case got.Depth < need.Depth:
		return fmt.Sprintf("depth %d < %d levels", got.Depth, need.Depth)
	}
	return ""
}

func describeBookCoverageGaps(req feedRequirements, books []feedDescribeBook) []string {
	served := make(map[feedObservationKey]feedDescribeBook, len(books))
	for _, b := range books {
		served[feedObservationKey{Host: b.Host, Namespace: b.Namespace, Coin: b.Coin, Kind: b.Kind}] = b
	}
	var gaps []string
	for _, k := range sortedBookNeedKeys(req.Books) {
		got, ok := served[k]
		if !ok {
			gaps = append(gaps, k.PayloadID()+" not served")
			continue
		}
		if why := describedBookGap(req.Books[k], got); why != "" {
			gaps = append(gaps, k.PayloadID()+" "+why)
		}
	}
	return gaps
}

func applyBookSealCoverage(snap *marketSnapshot, reqs cycleMarketRequirements) []string {
	var gaps []string
	for _, k := range sortedBookNeedKeys(reqs.Books) {
		need := reqs.Books[k]
		e, why := snap.bookEntry(k, need)
		if why != "" {
			gaps = append(gaps, why)
			continue
		}
		if !e.prefixComplete(need.WindowMs) {
			gaps = append(gaps, fmt.Sprintf("%s history is complete only from %d, after the window start %d", k.PayloadID(), e.CoverageFromMs, e.CutoffMs-need.WindowMs))
		}
	}
	return gaps
}
