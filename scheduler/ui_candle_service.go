package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	candleOriginPublished = "published"
	candleOriginArchive   = "archive"
	candleOriginTrades    = "trades"

	candleSourceClassMarket    = "market"
	candleSourceClassSynthetic = "synthetic"

	candleAvailSourceMissing      = "source_missing"
	candleAvailSourceLoading      = "source_loading"
	candleAvailSourceStale        = "source_stale"
	candleAvailSourceIncompatible = "source_incompatible"
	candleAvailWindowUncovered    = "window_uncovered"

	candleBoundaryHistoryStart = "history_start"
	candleBoundaryArchiveStart = "archive_start"
	candleBoundarySourceGap    = "source_gap"
	candleBoundaryReadBound    = "read_bound"
	candleBoundaryUnsupported  = "unsupported"

	candleFreshnessFresh = "fresh"
	candleFreshnessStale = "stale"

	candleServiceSlots       = 4
	candleServiceQueue       = 16
	candleServiceQueueWait   = 2 * time.Second
	candleServiceRetryAfter  = 2
	candleServiceDeadline    = 10 * time.Second
	candleLRUMaxEntries      = 256
	candleLRUMaxBytes        = 32 << 20
	candleLRUTTL             = 30 * time.Second
	candleDisplayDefaultLim  = 300
	candleDisplayMaxLimit    = 1000
	candleCursorVersion      = 1
	candleCursorMaxBytes     = 512
	candleDisplayMaxSpanBars = 1 << 20
)

var (
	errCandleBusy           = errors.New("candle reads are at their local bound")
	errCandleArchiveRetired = errors.New("the archive generation was retired during the read")
)

type candleServiceError struct {
	Status     int
	Code       string
	Detail     string
	RetryAfter int
	Caps       *candleCapabilities
}

func (e *candleServiceError) Error() string { return e.Code + ": " + e.Detail }

func candleErr(status int, code, format string, args ...interface{}) *candleServiceError {
	return &candleServiceError{Status: status, Code: code, Detail: fmt.Sprintf(format, args...)}
}

type candleAvailability struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type candleTimeframeCapability struct {
	Timeframe   string `json:"timeframe"`
	Origin      string `json:"origin"`
	History     bool   `json:"history"`
	Incremental bool   `json:"incremental"`
}

type candleCapabilities struct {
	DefaultTimeframe string                      `json:"default_timeframe"`
	Timeframes       []candleTimeframeCapability `json:"timeframes"`
}

func (c candleCapabilities) find(tf string) (candleTimeframeCapability, bool) {
	for _, t := range c.Timeframes {
		if t.Timeframe == tf {
			return t, true
		}
	}
	return candleTimeframeCapability{}, false
}

type candlePageView struct {
	Older     string `json:"older,omitempty"`
	Boundary  string `json:"boundary,omitempty"`
	Detail    string `json:"detail,omitempty"`
	ReadBound bool   `json:"read_bound,omitempty"`
}

type candleDisplayResponse struct {
	StrategyID         string              `json:"strategy_id"`
	Source             string              `json:"source"`
	Candles            []UICandle          `json:"candles"`
	Timeframe          string              `json:"timeframe"`
	NativeTimeframe    string              `json:"native_timeframe"`
	BarSeconds         int64               `json:"bar_seconds"`
	SourceClass        string              `json:"source_class"`
	Origin             string              `json:"origin"`
	Generation         string              `json:"generation,omitempty"`
	AsOf               string              `json:"as_of,omitempty"`
	Freshness          string              `json:"freshness"`
	FreshnessDetail    string              `json:"freshness_detail,omitempty"`
	Identity           *candleIdentity     `json:"identity,omitempty"`
	SeriesToken        string              `json:"series_token"`
	FormingTime        int64               `json:"forming_time,omitempty"`
	Page               candlePageView      `json:"page"`
	MarketAvailability *candleAvailability `json:"market_availability,omitempty"`
	Capabilities       candleCapabilities  `json:"capabilities"`
}

type candleCursor struct {
	V      int    `json:"v"`
	Class  string `json:"c"`
	TF     string `json:"tf"`
	ID     string `json:"id"`
	Gen    string `json:"g"`
	Before int64  `json:"b"`
}

func encodeCandleCursor(c candleCursor) string {
	c.V = candleCursorVersion
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCandleCursor(s string) (candleCursor, error) {
	var c candleCursor
	if len(s) == 0 || len(s) > candleCursorMaxBytes {
		return c, fmt.Errorf("cursor length is outside 1..%d", candleCursorMaxBytes)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("cursor is not base64url")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("cursor does not decode")
	}
	if c.V != candleCursorVersion || c.Before <= 0 || c.TF == "" || c.ID == "" ||
		(c.Class != candleSourceClassMarket && c.Class != candleSourceClassSynthetic) {
		return c, fmt.Errorf("cursor fields are invalid")
	}
	return c, nil
}

type candleDisplayRequest struct {
	Timeframe string
	Limit     int
	From      int64
	To        int64
	Cursor    *candleCursor
	CursorRaw string
}

type candleStrategyCapture struct {
	ID        string
	StorageID string
	Partition string
	Platform  string
	Type      string
	Symbol    string
	Timeframe string
}

func captureCandleStrategy(sc StrategyConfig) candleStrategyCapture {
	return candleStrategyCapture{
		ID: sc.ID, StorageID: effectiveStorageStrategyID(sc), Partition: partitionFor(sc).String(),
		Platform: sc.Platform, Type: sc.Type, Symbol: strategyDisplaySymbol(sc), Timeframe: strategyDisplayTimeframe(sc),
	}
}

type candleLRUEntry struct {
	key     string
	page    candlePage
	size    int
	expires time.Time
	elem    *list.Element
}

type candleCall struct {
	done chan struct{}
	page candlePage
	err  error
}

type candleService struct {
	published *publishedCandleStore
	archive   *candleArchiveLoader
	store     *StateStore
	strategy  func(string) (StrategyConfig, bool)
	now       func() time.Time

	slots chan struct{}
	queue chan struct{}

	mu         sync.Mutex
	inflight   map[string]*candleCall
	cache      map[string]*candleLRUEntry
	lru        *list.List
	cacheBytes int
	epoch      uint64

	served   atomic.Int64
	hits     atomic.Int64
	refused  atomic.Int64
	sourceOp atomic.Int64
}

func newCandleService(published *publishedCandleStore, archive *candleArchiveLoader, store *StateStore, strategy func(string) (StrategyConfig, bool)) *candleService {
	s := &candleService{
		published: published,
		archive:   archive,
		store:     store,
		strategy:  strategy,
		now:       time.Now,
		slots:     make(chan struct{}, candleServiceSlots),
		queue:     make(chan struct{}, candleServiceSlots+candleServiceQueue),
		inflight:  make(map[string]*candleCall),
		cache:     make(map[string]*candleLRUEntry),
		lru:       list.New(),
	}
	if archive != nil {
		archive.mu.Lock()
		archive.onSwap = s.dropArchiveEntries
		archive.mu.Unlock()
	}
	return s
}

type candleServiceStatus struct {
	PublishedIdentities int    `json:"published_identities"`
	PublishedReady      int    `json:"published_ready"`
	Served              int64  `json:"served"`
	CacheHits           int64  `json:"cache_hits"`
	Refused             int64  `json:"refused_busy"`
	SourceReads         int64  `json:"source_reads"`
	InFlightLimit       int    `json:"in_flight_limit"`
	QueueLimit          int    `json:"queue_limit"`
	CacheEntries        int    `json:"cache_entries"`
	RosterEpoch         uint64 `json:"roster_epoch"`
}

func (s *candleService) status() candleServiceStatus {
	if s == nil {
		return candleServiceStatus{}
	}
	ids, ready := s.published.count()
	s.mu.Lock()
	entries, epoch := len(s.cache), s.epoch
	s.mu.Unlock()
	return candleServiceStatus{PublishedIdentities: ids, PublishedReady: ready, Served: s.served.Load(), CacheHits: s.hits.Load(),
		Refused: s.refused.Load(), SourceReads: s.sourceOp.Load(), InFlightLimit: candleServiceSlots, QueueLimit: candleServiceQueue,
		CacheEntries: entries, RosterEpoch: epoch}
}

func (s *candleService) rosterChanged() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	s.dropPrefixLocked(candleOriginTrades + "|")
}

func (s *candleService) rosterEpoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

func (s *candleService) dropArchiveEntries() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPrefixLocked(candleOriginArchive + "|")
}

func (s *candleService) dropPrefixLocked(prefix string) {
	for k, e := range s.cache {
		if strings.HasPrefix(k, prefix) {
			s.lru.Remove(e.elem)
			s.cacheBytes -= e.size
			delete(s.cache, k)
		}
	}
}

func (s *candleService) cacheGetLocked(key string) (candlePage, bool) {
	e, ok := s.cache[key]
	if !ok {
		return candlePage{}, false
	}
	if !s.now().Before(e.expires) {
		s.lru.Remove(e.elem)
		s.cacheBytes -= e.size
		delete(s.cache, key)
		return candlePage{}, false
	}
	s.lru.MoveToFront(e.elem)
	return e.page, true
}

func (s *candleService) cacheSetLocked(key string, page candlePage) {
	size := len(key) + 64*len(page.Bars) + 128
	if size > candleLRUMaxBytes/4 {
		return
	}
	if old, ok := s.cache[key]; ok {
		s.lru.Remove(old.elem)
		s.cacheBytes -= old.size
		delete(s.cache, key)
	}
	e := &candleLRUEntry{key: key, page: page, size: size, expires: s.now().Add(candleLRUTTL)}
	e.elem = s.lru.PushFront(e)
	s.cache[key] = e
	s.cacheBytes += size
	for (len(s.cache) > candleLRUMaxEntries || s.cacheBytes > candleLRUMaxBytes) && s.lru.Len() > 0 {
		back := s.lru.Back()
		old := back.Value.(*candleLRUEntry)
		s.lru.Remove(back)
		s.cacheBytes -= old.size
		delete(s.cache, old.key)
	}
}

func (s *candleService) admit(ctx context.Context) (func(), error) {
	select {
	case s.queue <- struct{}{}:
	default:
		s.refused.Add(1)
		return nil, errCandleBusy
	}
	timer := time.NewTimer(candleServiceQueueWait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return func() {
			<-s.slots
			<-s.queue
		}, nil
	case <-timer.C:
		<-s.queue
		s.refused.Add(1)
		return nil, errCandleBusy
	case <-ctx.Done():
		<-s.queue
		return nil, ctx.Err()
	}
}

func (s *candleService) joinLocked(key string, cacheable bool) (candlePage, *candleCall, bool) {
	if cacheable {
		if page, ok := s.cacheGetLocked(key); ok {
			s.hits.Add(1)
			return page, nil, true
		}
	}
	if call, ok := s.inflight[key]; ok {
		return candlePage{}, call, true
	}
	return candlePage{}, nil, false
}

func (s *candleService) execute(ctx context.Context, key string, cacheable bool, run func(context.Context) (candlePage, error)) (candlePage, error) {
	wait := func(call *candleCall) (candlePage, error) {
		select {
		case <-call.done:
			return call.page, call.err
		case <-ctx.Done():
			return candlePage{}, ctx.Err()
		}
	}
	s.mu.Lock()
	page, call, ok := s.joinLocked(key, cacheable)
	s.mu.Unlock()
	if ok {
		if call != nil {
			return wait(call)
		}
		return page, nil
	}
	release, err := s.admit(ctx)
	if err != nil {
		return candlePage{}, err
	}
	s.mu.Lock()
	page, call, ok = s.joinLocked(key, cacheable)
	if ok {
		s.mu.Unlock()
		release()
		if call != nil {
			return wait(call)
		}
		return page, nil
	}
	call = &candleCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()
	go func() {
		workCtx, cancel := context.WithTimeout(context.Background(), candleServiceDeadline)
		s.sourceOp.Add(1)
		page, err := run(workCtx)
		cancel()
		release()
		call.page, call.err = page, err
		s.mu.Lock()
		if err == nil && cacheable {
			s.cacheSetLocked(key, page)
		}
		delete(s.inflight, key)
		s.mu.Unlock()
		close(call.done)
	}()
	reqCtx, cancel := context.WithTimeout(ctx, candleServiceDeadline)
	defer cancel()
	select {
	case <-call.done:
		return call.page, call.err
	case <-reqCtx.Done():
		return candlePage{}, reqCtx.Err()
	}
}

func candleWorkKey(parts ...interface{}) string {
	text := make([]string, len(parts))
	for i, p := range parts {
		text[i] = fmt.Sprint(p)
	}
	return strings.Join(text, "|")
}

func candleKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

func (s *candleService) capabilities(sc StrategyConfig, base candleIdentity, baseOK bool, gen *candleArchiveGeneration) candleCapabilities {
	native := strategyDisplayTimeframe(sc)
	caps := candleCapabilities{DefaultTimeframe: native}
	origins := make(map[string]candleTimeframeCapability)
	var order []string
	add := func(tf, origin string, history, incremental bool) {
		if _, ok := origins[tf]; ok {
			return
		}
		origins[tf] = candleTimeframeCapability{Timeframe: tf, Origin: origin, History: history, Incremental: incremental}
		order = append(order, tf)
	}
	if baseOK {
		series := base.series()
		archived := make(map[string]bool)
		for _, tf := range gen.timeframes(series) {
			archived[tf] = true
		}
		for _, tf := range s.published.timeframes(series) {
			add(tf, candleOriginPublished, archived[tf] || tf != candleDisplayOnlyMonth, tf != candleDisplayOnlyMonth)
		}
		for _, tf := range gen.timeframes(series) {
			add(tf, candleOriginArchive, true, false)
		}
	}
	if native != "" {
		if _, ok := origins[native]; !ok {
			_, grid := candleGridInterval(native)
			add(native, candleOriginTrades, grid, grid)
		}
	}
	sortCandleTimeframes(order)
	if i := indexOfString(order, native); i > 0 {
		order = append([]string{native}, append(order[:i:i], order[i+1:]...)...)
	}
	for _, tf := range order {
		caps.Timeframes = append(caps.Timeframes, origins[tf])
	}
	if caps.Timeframes == nil {
		caps.Timeframes = []candleTimeframeCapability{}
	}
	return caps
}

func indexOfString(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

func candleBarSeconds(tf string) int64 {
	if sec, ok := candleGridInterval(tf); ok {
		return sec
	}
	if tf == candleDisplayOnlyMonth {
		return 2_678_400
	}
	return 0
}

func (s *candleService) seriesToken(parts ...string) string {
	return candleKeyDigest(strings.Join(parts, "|"))
}

func (s *candleService) archiveAvailability(id candleIdentity, gen *candleArchiveGeneration) candleAvailability {
	if gen == nil {
		code, detail := s.archive.availability()
		if code == "" {
			code, detail = candleAvailSourceMissing, "no archive generation is admitted"
		}
		return candleAvailability{Code: code, Detail: detail}
	}
	if gen.lookup(id) == nil {
		return candleAvailability{Code: candleAvailSourceMissing, Detail: fmt.Sprintf("archive generation %s holds no series for %s", gen.ID, id)}
	}
	return candleAvailability{}
}

func archiveLatestPage(ctx context.Context, a *candleArchiveSeries, to int64, limit int) (candlePage, error) {
	hiIdx := a.Bars - 1
	if to > 0 {
		hiIdx = a.indexAtOrBefore(to)
	}
	if hiIdx < 0 {
		return candlePage{Bars: []UICandle{}}, nil
	}
	seg := a.Segments[a.segmentOfIndex(hiIdx)]
	lo := hiIdx - int64(limit) + 1
	if lo < seg.Start {
		lo = seg.Start
	}
	bars, err := a.readRange(ctx, lo, hiIdx+1)
	if err != nil {
		return candlePage{}, err
	}
	return archiveOlderInfo(a, lo, bars), nil
}

func archiveFromPage(ctx context.Context, a *candleArchiveSeries, from, to int64, limit int) (candlePage, error) {
	loIdx := a.indexAtOrAfter(from)
	if loIdx >= a.Bars {
		return candlePage{Bars: []UICandle{}}, nil
	}
	seg := a.Segments[a.segmentOfIndex(loIdx)]
	hi := seg.Start + seg.Bars
	if to > 0 {
		if lim := a.indexAtOrBefore(to) + 1; lim < hi {
			hi = lim
		}
	}
	if hi > loIdx+int64(limit) {
		hi = loIdx + int64(limit)
	}
	if hi <= loIdx {
		return candlePage{Bars: []UICandle{}}, nil
	}
	bars, err := a.readRange(ctx, loIdx, hi)
	if err != nil {
		return candlePage{}, err
	}
	return archiveOlderInfo(a, loIdx, bars), nil
}

func archiveBeforePage(ctx context.Context, a *candleArchiveSeries, before int64, limit int) (candlePage, error) {
	idx := a.indexAtOrBefore(before - 1)
	if idx < 0 {
		boundary := candleBoundaryArchiveStart
		if a.HistoryStartKnown {
			boundary = candleBoundaryHistoryStart
		}
		return candlePage{Bars: []UICandle{}, Boundary: boundary, Detail: "no archived bar opens before this page"}, nil
	}
	seg := a.Segments[a.segmentOfIndex(idx)]
	newest := seg.FirstOpen + (idx-seg.Start)*a.IntervalSec
	if newest != before-a.IntervalSec {
		return candlePage{Bars: []UICandle{}, Boundary: candleBoundarySourceGap,
			Detail: fmt.Sprintf("the archive holds no bar at %d, one bar before this page; a discontinuous series is not joined", before-a.IntervalSec)}, nil
	}
	lo := idx - int64(limit) + 1
	if lo < seg.Start {
		lo = seg.Start
	}
	bars, err := a.readRange(ctx, lo, idx+1)
	if err != nil {
		return candlePage{}, err
	}
	return archiveOlderInfo(a, lo, bars), nil
}

func archiveOlderInfo(a *candleArchiveSeries, lo int64, bars []UICandle) candlePage {
	page := candlePage{Bars: bars}
	if len(bars) == 0 {
		return page
	}
	segIdx := a.segmentOfIndex(lo)
	seg := a.Segments[segIdx]
	switch {
	case lo > seg.Start:
		page.Older = true
		page.Before = bars[0].Time
	case segIdx > 0:
		page.Boundary = candleBoundarySourceGap
		page.Detail = "the archive has a gap before this page; a discontinuous series is not joined"
	case a.HistoryStartKnown:
		page.Boundary = candleBoundaryHistoryStart
		page.Detail = "this page holds the first bar the venue serves"
	default:
		page.Boundary = candleBoundaryArchiveStart
		page.Detail = "this page holds the first archived bar"
	}
	return page
}

func publishedSelect(p *publishedCandleSeries, from, to, before int64, limit int) []UICandle {
	bars := p.Bars
	lo, hi := 0, len(bars)
	for lo < hi && from > 0 && bars[lo].Time < from {
		lo++
	}
	for hi > lo && ((to > 0 && bars[hi-1].Time > to) || (before > 0 && bars[hi-1].Time >= before)) {
		hi--
	}
	if from > 0 {
		if hi-lo > limit {
			hi = lo + limit
		}
	} else if hi-lo > limit {
		lo = hi - limit
	}
	out := make([]UICandle, hi-lo)
	copy(out, bars[lo:hi])
	return out
}

func publishedCovers(p *publishedCandleSeries, from, to int64) bool {
	if p == nil || len(p.Bars) == 0 {
		return false
	}
	if from > 0 && from < p.Bars[0].Time {
		return false
	}
	if from > 0 && from > p.Bars[len(p.Bars)-1].Time {
		return false
	}
	if to > 0 && to < p.Bars[0].Time {
		return false
	}
	return true
}

func publishedToken(p *publishedCandleSeries) string {
	return fmt.Sprintf("feed-%d:%s:%d", p.FeedGeneration, p.EvaluationID, p.SealedAt.UnixNano())
}

func (s *candleService) displayBars(ctx context.Context, sc StrategyConfig, req candleDisplayRequest) (*candleDisplayResponse, string, *candleServiceError) {
	capture := captureCandleStrategy(sc)
	epoch := s.rosterEpoch()
	native := capture.Timeframe
	tf := req.Timeframe
	if tf == "" {
		tf = native
	}
	if tf == "" {
		return nil, "", candleErr(http.StatusBadRequest, "timeframe_invalid", "strategy %s has no configured timeframe", sc.ID)
	}
	if !candleTimeframeKnown(tf) {
		return nil, "", candleErr(http.StatusBadRequest, "timeframe_invalid", "timeframe %q is not a known bar size", tf)
	}
	base, baseErr := candleIdentityFor(sc, capture.Symbol, native)
	baseOK := baseErr == nil
	gen := s.archive.pin()
	if gen != nil {
		defer gen.release()
	}
	caps := s.capabilities(sc, base, baseOK, gen)
	if _, ok := caps.find(tf); !ok {
		e := candleErr(http.StatusBadRequest, "timeframe_unsupported", "timeframe %s has no market source for this strategy and is not its configured timeframe", tf)
		e.Caps = &caps
		return nil, "", e
	}
	limit := req.Limit
	if limit <= 0 {
		limit = candleDisplayDefaultLim
	}
	if limit > candleDisplayMaxLimit {
		limit = candleDisplayMaxLimit
	}
	id := base.withTimeframe(tf)
	resp := &candleDisplayResponse{StrategyID: sc.ID, Timeframe: tf, NativeTimeframe: native, BarSeconds: candleBarSeconds(tf), Capabilities: caps}
	genID := ""
	if gen != nil {
		genID = gen.ID
	}
	now := s.now().UTC()
	var pub *publishedCandleSeries
	var arch *candleArchiveSeries
	if baseOK {
		pub = s.published.get(id)
		if pub != nil && len(pub.Bars) == 0 {
			pub = nil
		}
		if gen != nil {
			arch = gen.lookup(id)
		}
	}
	marketAvail := func() *candleAvailability {
		if !baseOK {
			return &candleAvailability{Code: candleAvailSourceIncompatible, Detail: baseErr.Error()}
		}
		if pub != nil || arch != nil {
			return &candleAvailability{Code: candleAvailWindowUncovered, Detail: "no market source holds bars for the requested window"}
		}
		a := s.archiveAvailability(id, gen)
		if latest := s.published.get(id); latest != nil && latest.Latest.Status != "" && !latest.Latest.Ready {
			a.Detail = fmt.Sprintf("feed key %s: %s %s; archive: %s", id, latest.Latest.Status, latest.Latest.Detail, a.Detail)
		}
		return &a
	}
	var key string
	var page candlePage
	var err error
	var origin string
	switch {
	case req.Cursor != nil:
		c := req.Cursor
		if c.TF != tf {
			return nil, "", candleErr(http.StatusBadRequest, "cursor_invalid", "the cursor names timeframe %s, the request %s", c.TF, tf)
		}
		if c.Class == candleSourceClassSynthetic {
			if c.ID != s.syntheticDigest(capture, tf) {
				return nil, "", candleErr(http.StatusConflict, "cursor_expired", "the cursor belongs to another strategy source; restart from the newest bars")
			}
			origin = candleOriginTrades
			break
		}
		if !baseOK || c.ID != id.digest() {
			return nil, "", candleErr(http.StatusConflict, "cursor_expired", "the cursor belongs to another market identity; restart from the newest bars")
		}
		if c.Gen != genID {
			return nil, "", candleErr(http.StatusConflict, "cursor_expired", "the archive generation changed from %q to %q; restart from the newest bars", c.Gen, genID)
		}
		if pub != nil && pub.Bars[0].Time < c.Before {
			origin = candleOriginPublished
		} else if arch != nil {
			origin = candleOriginArchive
		} else {
			origin = ""
			page = candlePage{Bars: []UICandle{}, Boundary: candleBoundaryReadBound, Detail: "older bars need an archive series; the feed holds only its own lookback"}
			if pub == nil {
				page.Boundary = candleBoundaryUnsupported
				page.Detail = "no market source serves older bars for this timeframe"
			}
		}
	case pub != nil && publishedCovers(pub, req.From, req.To):
		origin = candleOriginPublished
	case arch != nil:
		origin = candleOriginArchive
	case tf == native && capture.Symbol != "" && s.store != nil:
		origin = candleOriginTrades
	}
	switch origin {
	case candleOriginPublished:
		before := int64(0)
		if req.Cursor != nil {
			before = req.Cursor.Before
		}
		key = candleWorkKey(candleOriginPublished, publishedToken(pub), id, req.From, req.To, before, limit)
		p := pub
		page, err = s.execute(ctx, key, false, func(context.Context) (candlePage, error) {
			bars := publishedSelect(p, req.From, req.To, before, limit)
			pg := candlePage{Bars: bars}
			if before > 0 && len(bars) > 0 && bars[len(bars)-1].Time != before-int64(p.IntervalMs/1000) {
				return candlePage{Bars: []UICandle{}, Boundary: candleBoundarySourceGap, Detail: "the feed holds no bar one bar before this page"}, nil
			}
			if p.FormingOpen != 0 && len(bars) > 0 && bars[len(bars)-1].Time == p.FormingOpen {
				pg.FormingTime = p.FormingOpen
			}
			if len(bars) > 0 {
				if bars[0].Time > p.Bars[0].Time || arch != nil {
					pg.Older = true
					pg.Before = bars[0].Time
				} else {
					pg.Boundary = candleBoundaryReadBound
					pg.Detail = "the feed holds only its own lookback and no archive series covers older bars"
				}
			}
			return pg, nil
		})
		if err == nil {
			resp.Origin, resp.SourceClass = candleOriginPublished, candleSourceClassMarket
			resp.Source = candleVenueHyperliquid + ":feed:" + pub.Provenance
			resp.Generation = fmt.Sprintf("feed-%d:%s", pub.FeedGeneration, pub.EvaluationID)
			resp.AsOf = pub.SealedAt.Format(time.RFC3339)
			resp.Freshness = candleFreshnessFresh
			if stale, why := pub.stale(now); stale {
				resp.Freshness, resp.FreshnessDetail = candleFreshnessStale, why
			}
			resp.SeriesToken = s.seriesToken(candleSourceClassMarket, id.String())
		}
	case candleOriginArchive:
		a := arch
		g := gen
		mode := "latest"
		before := int64(0)
		switch {
		case req.Cursor != nil:
			mode, before = "before", req.Cursor.Before
		case req.From > 0:
			mode = "from"
		}
		key = candleWorkKey(candleOriginArchive, g.ID, id, mode, req.From, req.To, before, limit)
		page, err = s.execute(ctx, key, true, func(workCtx context.Context) (candlePage, error) {
			if !g.acquire() {
				return candlePage{}, errCandleArchiveRetired
			}
			defer g.release()
			switch mode {
			case "before":
				return archiveBeforePage(workCtx, a, before, limit)
			case "from":
				return archiveFromPage(workCtx, a, req.From, req.To, limit)
			}
			return archiveLatestPage(workCtx, a, req.To, limit)
		})
		if err == nil {
			if len(page.Bars) == 0 && req.Cursor == nil {
				resp.MarketAvailability = &candleAvailability{Code: candleAvailWindowUncovered, Detail: "the archive holds no bar in the requested window"}
			}
			resp.Origin, resp.SourceClass = candleOriginArchive, candleSourceClassMarket
			resp.Source = "archive:" + g.ID + ":" + a.SourceKind
			resp.Generation = g.ID
			resp.AsOf = g.PublishedAt.Format(time.RFC3339)
			resp.Freshness = candleFreshnessFresh
			if g.expired(now) {
				resp.Freshness, resp.FreshnessDetail = candleFreshnessStale, "archive generation "+g.ID+" expired at "+g.ExpiresAt.Format(time.RFC3339)
			}
			resp.SeriesToken = s.seriesToken(candleSourceClassMarket, id.String())
		}
	case candleOriginTrades:
		before := int64(0)
		if req.Cursor != nil {
			before = req.Cursor.Before
		}
		key = candleWorkKey(candleOriginTrades, epoch, capture.ID, capture.StorageID, capture.Partition, capture.Symbol, tf, req.From, req.To, before, limit)
		page, err = s.execute(ctx, key, true, func(context.Context) (candlePage, error) {
			return syntheticCandlePage(s.store, sc, capture.Symbol, tf, req.From, req.To, before, limit)
		})
		if err == nil {
			resp.Origin, resp.SourceClass = candleOriginTrades, candleSourceClassSynthetic
			resp.Source = candleOriginTrades
			resp.AsOf = now.Format(time.RFC3339)
			resp.Freshness = candleFreshnessFresh
			resp.SeriesToken = s.seriesToken(candleSourceClassSynthetic, capture.ID, capture.StorageID, capture.Partition, tf)
			if req.Cursor == nil {
				resp.MarketAvailability = marketAvail()
			}
		}
	default:
		if req.Cursor == nil {
			page = candlePage{Bars: []UICandle{}}
			resp.MarketAvailability = marketAvail()
		}
		resp.Origin, resp.SourceClass = "", candleSourceClassMarket
		resp.Freshness = candleFreshnessStale
		resp.SeriesToken = s.seriesToken(candleSourceClassMarket, id.String())
	}
	if err != nil {
		return nil, "", s.serviceError(err)
	}
	if cur, ok := s.strategy(sc.ID); !ok || captureCandleStrategy(cur) != capture {
		return nil, "", candleErr(http.StatusConflict, "strategy_changed", "strategy %s changed during the read; reload the chart", sc.ID)
	}
	s.served.Add(1)
	if baseOK && resp.SourceClass == candleSourceClassMarket {
		idCopy := id
		resp.Identity = &idCopy
	}
	resp.Candles = page.Bars
	if resp.Candles == nil {
		resp.Candles = []UICandle{}
	}
	resp.FormingTime = page.FormingTime
	resp.Page = candlePageView{Boundary: page.Boundary, Detail: page.Detail, ReadBound: page.ReadBound}
	if page.Older && page.Before > 0 {
		c := candleCursor{TF: tf, Before: page.Before}
		if resp.SourceClass == candleSourceClassSynthetic {
			c.Class, c.ID = candleSourceClassSynthetic, s.syntheticDigest(capture, tf)
		} else {
			c.Class, c.ID, c.Gen = candleSourceClassMarket, id.digest(), genID
		}
		resp.Page.Older = encodeCandleCursor(c)
	}
	if key == "" {
		key = candleWorkKey("none", id, tf, req.From, req.To, req.CursorRaw, limit, genID)
	}
	return resp, candleKeyDigest(key), nil
}

func (s *candleService) syntheticDigest(c candleStrategyCapture, tf string) string {
	return candleKeyDigest(strings.Join([]string{candleSourceClassSynthetic, c.ID, c.StorageID, c.Partition, c.Symbol, tf}, "|"))
}

func (s *candleService) serviceError(err error) *candleServiceError {
	var se *candleServiceError
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, errCandleBusy):
		e := candleErr(http.StatusTooManyRequests, "busy", "dashboard candle reads are at their local in-flight bound; retry after %d seconds", candleServiceRetryAfter)
		e.RetryAfter = candleServiceRetryAfter
		return e
	case errors.Is(err, errCandleArchiveIntegrity), errors.Is(err, errCandleArchiveRetired):
		return candleErr(http.StatusServiceUnavailable, candleAvailSourceIncompatible, "%v", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return candleErr(http.StatusServiceUnavailable, candleAvailSourceLoading, "the candle read did not finish inside its deadline")
	}
	return candleErr(http.StatusInternalServerError, "read_failed", "%v", err)
}

type candlePreviewResult struct {
	Bars       []UICandle
	Origin     string
	Source     string
	Timeframe  string
	Generation string
}

func (s *candleService) previewBars(ctx context.Context, sc StrategyConfig, from, to int64, limit int) (candlePreviewResult, *candleServiceError) {
	capture := captureCandleStrategy(sc)
	tf := capture.Timeframe
	id, err := candleIdentityFor(sc, capture.Symbol, tf)
	if err != nil {
		return candlePreviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceIncompatible, "%v", err)
	}
	now := s.now().UTC()
	gen := s.archive.pin()
	if gen != nil {
		defer gen.release()
	}
	var best candlePreviewResult
	var reasons []string
	if p := s.published.get(id); p != nil && len(p.Bars) > 0 {
		if stale, why := p.stale(now); stale {
			reasons = append(reasons, "feed: "+why)
		} else if from > 0 && !publishedCovers(p, from, to) {
			reasons = append(reasons, "feed: the window is outside the held bars")
		} else {
			pp := p
			page, err := s.execute(ctx, candleWorkKey("preview", candleOriginPublished, publishedToken(pp), id, from, to, limit), false, func(context.Context) (candlePage, error) {
				closed := *pp
				closed.Bars = pp.closedBars()
				return candlePage{Bars: publishedSelect(&closed, from, to, 0, limit)}, nil
			})
			if err != nil {
				return candlePreviewResult{}, s.serviceError(err)
			}
			best = candlePreviewResult{Bars: page.Bars, Origin: candleOriginPublished, Source: candleVenueHyperliquid + ":feed:" + pp.Provenance,
				Timeframe: tf, Generation: fmt.Sprintf("feed-%d:%s", pp.FeedGeneration, pp.EvaluationID)}
		}
	} else if p != nil && p.Latest.Status != "" {
		reasons = append(reasons, "feed: "+p.Latest.Status+" "+p.Latest.Detail)
	}
	if gen != nil {
		if a := gen.lookup(id); a == nil {
			reasons = append(reasons, "archive: no series for "+id.String())
		} else if gen.expired(now) {
			reasons = append(reasons, "archive: generation "+gen.ID+" expired")
		} else {
			g := gen
			mode := "latest"
			if from > 0 {
				mode = "from"
			}
			page, err := s.execute(ctx, candleWorkKey(candleOriginArchive, g.ID, id, "preview-"+mode, from, to, 0, limit), true, func(workCtx context.Context) (candlePage, error) {
				if !g.acquire() {
					return candlePage{}, errCandleArchiveRetired
				}
				defer g.release()
				if mode == "from" {
					return archiveFromPage(workCtx, a, from, to, limit)
				}
				return archiveLatestPage(workCtx, a, to, limit)
			})
			if err != nil {
				return candlePreviewResult{}, s.serviceError(err)
			}
			if len(page.Bars) > len(best.Bars) {
				best = candlePreviewResult{Bars: page.Bars, Origin: candleOriginArchive, Source: "archive:" + g.ID + ":" + a.SourceKind, Timeframe: tf, Generation: g.ID}
			}
		}
	} else {
		_, detail := s.archive.availability()
		reasons = append(reasons, "archive: "+detail)
	}
	if cur, ok := s.strategy(sc.ID); !ok || captureCandleStrategy(cur) != capture {
		return candlePreviewResult{}, candleErr(http.StatusConflict, "strategy_changed", "strategy %s changed during the read", sc.ID)
	}
	if len(best.Bars) == 0 {
		return candlePreviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceMissing,
			"no admissible native %s market bars for %s (%s); trade-built bars never enter a preview", tf, sc.ID, strings.Join(reasons, "; "))
	}
	return best, nil
}

type candleReviewProvenance struct {
	Origin     string
	Generation string
	Identity   candleIdentity
	Verified   bool
	Detail     string
	Source     string
}

type candleReviewResult struct {
	Bars       []UICandle
	Provenance candleReviewProvenance
}

func (s *candleService) reviewWindow(ctx context.Context, sc StrategyConfig, symbol string, w tradeReviewWindow) (candleReviewResult, *candleServiceError) {
	id, err := candleIdentityFor(sc, symbol, w.Timeframe)
	if err != nil {
		return candleReviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceIncompatible, "%v", err)
	}
	gen := s.archive.pin()
	if gen == nil {
		code, detail := s.archive.availability()
		if code == "" {
			code, detail = candleAvailSourceMissing, "no archive generation is admitted"
		}
		return candleReviewResult{}, candleErr(http.StatusServiceUnavailable, code, "%s", detail)
	}
	defer gen.release()
	now := s.now().UTC()
	if gen.expired(now) {
		return candleReviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceStale, "archive generation %s expired at %s", gen.ID, gen.ExpiresAt.Format(time.RFC3339))
	}
	a := gen.lookup(id)
	if a == nil {
		return candleReviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceMissing, "archive generation %s holds no series for %s", gen.ID, id)
	}
	if a.IntervalSec != w.BarSeconds {
		return candleReviewResult{}, candleErr(http.StatusServiceUnavailable, candleAvailSourceIncompatible, "archive series %s has %d-second bars, the window needs %d", id, a.IntervalSec, w.BarSeconds)
	}
	prov := candleReviewProvenance{Origin: candleOriginArchive, Generation: gen.ID, Identity: id, Source: "archive:" + gen.ID + ":" + a.SourceKind}
	segFrom, okFrom := a.segmentAt(w.EffectiveFrom)
	segTo, okTo := a.segmentAt(w.EffectiveTo)
	switch {
	case w.EffectiveTo > a.FinalizedThrough:
		prov.Detail = fmt.Sprintf("archive generation %s is finalized through %d, before the window end %d", gen.ID, a.FinalizedThrough, w.EffectiveTo)
	case !okFrom || !okTo || segFrom != segTo:
		prov.Detail = fmt.Sprintf("the window %d..%d is not inside one contiguous segment of archive generation %s", w.EffectiveFrom, w.EffectiveTo, gen.ID)
	default:
		prov.Verified = true
	}
	lo := a.indexAtOrAfter(w.EffectiveFrom)
	hi := a.indexAtOrBefore(w.EffectiveTo) + 1
	if hi-lo > diagMaxFetchBars+1 {
		hi = lo + diagMaxFetchBars + 1
	}
	key := candleWorkKey("review", candleOriginArchive, gen.ID, id, lo, hi)
	page, xerr := s.execute(ctx, key, false, func(workCtx context.Context) (candlePage, error) {
		if !gen.acquire() {
			return candlePage{}, errCandleArchiveRetired
		}
		defer gen.release()
		bars, err := a.readRange(workCtx, lo, hi)
		return candlePage{Bars: bars}, err
	})
	if xerr != nil {
		return candleReviewResult{}, s.serviceError(xerr)
	}
	return candleReviewResult{Bars: page.Bars, Provenance: prov}, nil
}

func (s *candleService) reviewGenerationCurrent(pinned string) (bool, string) {
	g := s.archive.generation()
	if g == nil {
		return false, "no archive generation is admitted now"
	}
	if g.ID != pinned {
		return false, fmt.Sprintf("the archive generation changed from %s to %s after validation", pinned, g.ID)
	}
	if g.expired(s.now()) {
		return false, fmt.Sprintf("archive generation %s expired after validation", g.ID)
	}
	return true, ""
}

func writeCandleServiceError(w http.ResponseWriter, e *candleServiceError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.WriteHeader(e.Status)
	body := map[string]interface{}{"error": e.Detail, "code": e.Code}
	if e.Caps != nil {
		body["capabilities"] = e.Caps
	}
	json.NewEncoder(w).Encode(body)
}
