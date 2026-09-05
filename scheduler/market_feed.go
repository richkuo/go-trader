package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hlFeedHistoryMargin    = 50
	feedMinReadyBars       = 30
	feedStaleGrace         = 60 * time.Second
	feedSilenceFloor       = 5 * time.Minute
	feedMidStaleAfter      = 15 * time.Second
	feedReconnectGrace     = 30 * time.Second
	feedDecisionAgeFloor   = 60 * time.Second
	feedPendingSocketCap   = 512
	feedAlertChannelDepth  = 128
	feedNamespacePerps     = "perps"
	feedBootstrapKeyBudget = 45 * time.Second
	feedStartupBudget      = 120 * time.Second
)

type feedBarSource string

const (
	feedBarSourceSocket feedBarSource = "ws"
	feedBarSourceREST   feedBarSource = "rest"
)

type feedKeyStatus string

const (
	feedStatusBootstrapping feedKeyStatus = "bootstrapping"
	feedStatusReady         feedKeyStatus = "ready"
	feedStatusShort         feedKeyStatus = "short"
	feedStatusStale         feedKeyStatus = "stale"
	feedStatusRepairing     feedKeyStatus = "repairing"
	feedStatusInvalid       feedKeyStatus = "invalid"
	feedStatusFailed        feedKeyStatus = "failed"
)

type marketFeedKey struct {
	Host      string
	Namespace string
	Symbol    string
	Timeframe string
}

func (k marketFeedKey) String() string {
	return k.Host + "|" + k.Namespace + "|" + k.Symbol + "|" + k.Timeframe
}

func (k marketFeedKey) PayloadID() string {
	return k.Symbol + "|" + k.Timeframe
}

func marketFeedKeyLess(a, b marketFeedKey) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Symbol != b.Symbol {
		return a.Symbol < b.Symbol
	}
	return a.Timeframe < b.Timeframe
}

func sortMarketFeedKeys(keys []marketFeedKey) {
	sort.Slice(keys, func(i, j int) bool { return marketFeedKeyLess(keys[i], keys[j]) })
}

type feedBar struct {
	OpenMs  int64
	CloseMs int64
	Open    float64
	High    float64
	Low     float64
	Close   float64
	Volume  float64
	Seq     uint64
	RecvAt  time.Time
	Source  feedBarSource
}

func (b feedBar) row() hlCandleRow {
	return hlCandleRow{
		TimestampMs: b.CloseMs,
		Open:        b.Open,
		High:        b.High,
		Low:         b.Low,
		Close:       b.Close,
		Volume:      b.Volume,
	}
}

func feedBarFromRaw(raw hlCandleRaw, source feedBarSource, recvAt time.Time) feedBar {
	return feedBar{
		OpenMs:  raw.OpenMs,
		CloseMs: raw.CloseMs,
		Open:    raw.Open,
		High:    raw.High,
		Low:     raw.Low,
		Close:   raw.Close,
		Volume:  raw.Volume,
		RecvAt:  recvAt,
		Source:  source,
	}
}

func validateFeedBar(bar feedBar, intervalMs int64) error {
	if bar.OpenMs <= 0 {
		return fmt.Errorf("open timestamp %d is not positive", bar.OpenMs)
	}
	if bar.CloseMs <= bar.OpenMs {
		return fmt.Errorf("close timestamp %d must be after the open timestamp %d", bar.CloseMs, bar.OpenMs)
	}
	if bar.CloseMs-bar.OpenMs > intervalMs {
		return fmt.Errorf("bar spans %dms, more than one %dms interval", bar.CloseMs-bar.OpenMs, intervalMs)
	}
	for name, v := range map[string]float64{"open": bar.Open, "high": bar.High, "low": bar.Low, "close": bar.Close} {
		if !isFiniteFeedPrice(v) {
			return fmt.Errorf("%s price %v is not a finite positive number", name, v)
		}
	}
	if math.IsNaN(bar.Volume) || math.IsInf(bar.Volume, 0) || bar.Volume < 0 {
		return fmt.Errorf("volume %v is negative or not finite", bar.Volume)
	}
	if bar.High < bar.Open || bar.High < bar.Close || bar.High < bar.Low {
		return fmt.Errorf("high %v is below open %v, close %v or low %v", bar.High, bar.Open, bar.Close, bar.Low)
	}
	if bar.Low > bar.Open || bar.Low > bar.Close {
		return fmt.Errorf("low %v is above open %v or close %v", bar.Low, bar.Open, bar.Close)
	}
	return nil
}

func isFiniteFeedPrice(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0
}

type feedKeyState struct {
	Key           marketFeedKey
	IntervalMs    int64
	Required      int
	Capacity      int
	Bars          []feedBar
	Status        feedKeyStatus
	StatusDetail  string
	CoverageShort bool
	LastRecvAt    time.Time
	LastSource    feedBarSource
	Pending       []feedBar
	InvalidCount  int
	LastInvalid   string
	seq           uint64
}

func newFeedKeyState(key marketFeedKey, intervalMs int64, required int) *feedKeyState {
	capacity := required + hlFeedHistoryMargin
	return &feedKeyState{
		Key:        key,
		IntervalMs: intervalMs,
		Required:   required,
		Capacity:   capacity,
		Status:     feedStatusBootstrapping,
	}
}

func (s *feedKeyState) raiseRequired(required int) {
	if required <= s.Required {
		return
	}
	s.Required = required
	s.Capacity = required + hlFeedHistoryMargin
}

func (s *feedKeyState) indexOfOpen(openMs int64) (int, bool) {
	i := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].OpenMs >= openMs })
	if i < len(s.Bars) && s.Bars[i].OpenMs == openMs {
		return i, true
	}
	return i, false
}

func (s *feedKeyState) insertAt(idx int, bar feedBar) {
	s.Bars = append(s.Bars, feedBar{})
	copy(s.Bars[idx+1:], s.Bars[idx:])
	s.Bars[idx] = bar
}

func (s *feedKeyState) trim() {
	if s.Capacity > 0 && len(s.Bars) > s.Capacity {
		s.Bars = append([]feedBar(nil), s.Bars[len(s.Bars)-s.Capacity:]...)
	}
}

func (s *feedKeyState) nextSeq() uint64 {
	s.seq++
	return s.seq
}

func (s *feedKeyState) recordInvalid(err error) {
	s.InvalidCount++
	s.LastInvalid = err.Error()
}

func applySocketBar(s *feedKeyState, bar feedBar) error {
	if err := validateFeedBar(bar, s.IntervalMs); err != nil {
		s.recordInvalid(err)
		return err
	}
	bar.Source = feedBarSourceSocket
	if s.Status == feedStatusBootstrapping {
		if len(s.Pending) >= feedPendingSocketCap {
			s.Pending = s.Pending[1:]
		}
		s.Pending = append(s.Pending, bar)
		return nil
	}
	storeSocketBar(s, bar)
	return nil
}

func storeSocketBar(s *feedKeyState, bar feedBar) {
	idx, found := s.indexOfOpen(bar.OpenMs)
	if found {
		if bar.CloseMs < s.Bars[idx].CloseMs {
			return
		}
		bar.Seq = s.nextSeq()
		s.Bars[idx] = bar
	} else {
		if len(s.Bars) >= s.Capacity && idx == 0 {
			return
		}
		bar.Seq = s.nextSeq()
		s.insertAt(idx, bar)
	}
	s.trim()
	if bar.RecvAt.After(s.LastRecvAt) {
		s.LastRecvAt = bar.RecvAt
	}
	s.LastSource = feedBarSourceSocket
}

func drainPendingSocketBars(s *feedKeyState) {
	pending := s.Pending
	s.Pending = nil
	for _, bar := range pending {
		storeSocketBar(s, bar)
	}
}

type mergeRestOutcome struct {
	Added    int
	Replaced int
	Kept     int
	Invalid  int
}

func mergeRestRows(s *feedKeyState, raws []hlCandleRaw, requestedAt time.Time) mergeRestOutcome {
	var out mergeRestOutcome
	for _, raw := range raws {
		bar := feedBarFromRaw(raw, feedBarSourceREST, requestedAt)
		if err := validateFeedBar(bar, s.IntervalMs); err != nil {
			s.recordInvalid(err)
			out.Invalid++
			continue
		}
		idx, found := s.indexOfOpen(bar.OpenMs)
		if found {
			if s.Bars[idx].RecvAt.After(requestedAt) {
				out.Kept++
				continue
			}
			bar.Seq = s.nextSeq()
			s.Bars[idx] = bar
			out.Replaced++
			continue
		}
		if len(s.Bars) >= s.Capacity && idx == 0 {
			continue
		}
		bar.Seq = s.nextSeq()
		s.insertAt(idx, bar)
		out.Added++
	}
	s.trim()
	if out.Added+out.Replaced > 0 {
		if requestedAt.After(s.LastRecvAt) {
			s.LastRecvAt = requestedAt
			s.LastSource = feedBarSourceREST
		}
	}
	return out
}

type feedKeyReadiness struct {
	Key           marketFeedKey
	Bars          int
	Required      int
	Ready         bool
	Stale         bool
	StaleReason   string
	CoverageShort bool
	Status        feedKeyStatus
	Detail        string
	FirstOpenMs   int64
	LastOpenMs    int64
	LastCloseMs   int64
	LastRecvAt    time.Time
	Source        string
}

func feedReadyFloor(required int) int {
	if required < feedMinReadyBars {
		return required
	}
	return feedMinReadyBars
}

func feedSilenceLimit(intervalMs int64) time.Duration {
	limit := time.Duration(2*intervalMs) * time.Millisecond
	if limit < feedSilenceFloor {
		return feedSilenceFloor
	}
	return limit
}

func keyReadiness(s *feedKeyState, now time.Time, connected bool) feedKeyReadiness {
	out := feedKeyReadiness{
		Key:           s.Key,
		Bars:          len(s.Bars),
		Required:      s.Required,
		Status:        s.Status,
		Detail:        s.StatusDetail,
		CoverageShort: s.CoverageShort,
		LastRecvAt:    s.LastRecvAt,
		Source:        string(s.LastSource),
	}
	if len(s.Bars) > 0 {
		out.FirstOpenMs = s.Bars[0].OpenMs
		out.LastOpenMs = s.Bars[len(s.Bars)-1].OpenMs
		out.LastCloseMs = s.Bars[len(s.Bars)-1].CloseMs
	}
	if s.Status == feedStatusFailed {
		return out
	}
	floor := feedReadyFloor(s.Required)
	if len(s.Bars) == 0 {
		out.Status = feedStatusBootstrapping
		out.Detail = "no bars"
		return out
	}
	if len(s.Bars) < floor {
		out.Status = feedStatusShort
		out.Detail = fmt.Sprintf("%d bars, below the %d-bar floor", len(s.Bars), floor)
		return out
	}
	out.CoverageShort = len(s.Bars) < s.Required
	nowMs := now.UTC().UnixMilli()
	if nowMs-out.LastCloseMs > 2*s.IntervalMs+int64(feedStaleGrace/time.Millisecond) {
		out.Stale = true
		out.StaleReason = fmt.Sprintf("newest bar closed %dms ago, over %d intervals",
			nowMs-out.LastCloseMs, 2)
	} else if connected && !s.LastRecvAt.IsZero() && now.Sub(s.LastRecvAt) > feedSilenceLimit(s.IntervalMs) {
		out.Stale = true
		out.StaleReason = fmt.Sprintf("no socket update for %s while connected", now.Sub(s.LastRecvAt).Round(time.Second))
	}
	if out.Stale {
		out.Status = feedStatusStale
		out.Detail = out.StaleReason
		return out
	}
	out.Ready = true
	out.Status = feedStatusReady
	if out.CoverageShort {
		out.Detail = fmt.Sprintf("venue history is shorter than required (%d of %d bars)", len(s.Bars), s.Required)
	} else {
		out.Detail = ""
	}
	return out
}

type feedMid struct {
	Px     float64
	RecvAt time.Time
	Source string
}

type feedFundingRecord struct {
	Rate   float64 `json:"rate"`
	TimeMs int64   `json:"time"`
}

type feedFunding struct {
	Current    float64
	Avg7d      float64
	HasScalar  bool
	Records    []feedFundingRecord
	HasRecords bool
	FetchedAt  time.Time
	Source     string
	Err        string
}

type feedRestReason string

const (
	feedRestBootstrap feedRestReason = "bootstrap"
	feedRestRepair    feedRestReason = "repair"
	feedRestRecovery  feedRestReason = "recovery"
	feedRestSteady    feedRestReason = "steady"
)

type feedMetrics struct {
	BootstrapCalls    int
	RepairCalls       int
	RecoveryCalls     int
	SteadyCandleCalls int
}

type feedAlert struct {
	Key    marketFeedKey
	Kind   string
	Detail string
	At     time.Time
}

type marketFeedOwner struct {
	feedMu sync.Mutex

	keys         map[marketFeedKey]*feedKeyState
	published    map[marketFeedKey]bool
	mids         map[string]feedMid
	midCoins     map[string]bool
	funding      map[string]*feedFunding
	fundingNeeds map[string]feedFundingNeed

	gen        uint64
	subVersion uint64

	connected        bool
	lastConnectAt    time.Time
	lastDisconnectAt time.Time

	metrics feedMetrics

	alerts chan feedAlert
	clock  func() time.Time
	logf   func(string, ...any)
}

type feedFundingNeed struct {
	Scalar  bool
	Records bool
	StartMs int64
}

func newMarketFeedOwner(clock func() time.Time, logf func(string, ...any)) *marketFeedOwner {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &marketFeedOwner{
		keys:         make(map[marketFeedKey]*feedKeyState),
		published:    make(map[marketFeedKey]bool),
		mids:         make(map[string]feedMid),
		midCoins:     make(map[string]bool),
		funding:      make(map[string]*feedFunding),
		fundingNeeds: make(map[string]feedFundingNeed),
		alerts:       make(chan feedAlert, feedAlertChannelDepth),
		clock:        clock,
		logf:         logf,
	}
}

func (o *marketFeedOwner) now() time.Time {
	return o.clock().UTC()
}

func (o *marketFeedOwner) raiseAlert(key marketFeedKey, kind, detail string) {
	select {
	case o.alerts <- feedAlert{Key: key, Kind: kind, Detail: detail, At: o.now()}:
	default:
	}
}

func (o *marketFeedOwner) DrainAlerts() []feedAlert {
	var out []feedAlert
	for {
		select {
		case a := <-o.alerts:
			out = append(out, a)
		default:
			return out
		}
	}
}

func (o *marketFeedOwner) countRestCall(reason feedRestReason) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	switch reason {
	case feedRestBootstrap:
		o.metrics.BootstrapCalls++
	case feedRestRepair:
		o.metrics.RepairCalls++
	case feedRestRecovery:
		o.metrics.RecoveryCalls++
	default:
		o.metrics.SteadyCandleCalls++
	}
}

func (o *marketFeedOwner) Metrics() feedMetrics {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.metrics
}

func (o *marketFeedOwner) Generation() uint64 {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.gen
}

func (o *marketFeedOwner) SubscriptionVersion() uint64 {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.subVersion
}

func (o *marketFeedOwner) Subscriptions() ([]marketFeedKey, []string, uint64) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	keys := make([]marketFeedKey, 0, len(o.keys))
	for k := range o.keys {
		keys = append(keys, k)
	}
	sortMarketFeedKeys(keys)
	coins := make([]string, 0, len(o.midCoins))
	for c := range o.midCoins {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return keys, coins, o.subVersion
}

func (o *marketFeedOwner) SetConnected(connected bool) {
	o.feedMu.Lock()
	changed := o.connected != connected
	o.connected = connected
	now := o.now()
	if changed {
		if connected {
			o.lastConnectAt = now
		} else {
			o.lastDisconnectAt = now
			for _, st := range o.keys {
				if st.Status == feedStatusReady || st.Status == feedStatusStale {
					st.Status = feedStatusRepairing
					st.StatusDetail = "socket disconnected"
				}
			}
		}
	}
	o.feedMu.Unlock()
}

func (o *marketFeedOwner) Connected() bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.connected
}

func (o *marketFeedOwner) IngestSocketCandle(key marketFeedKey, raw hlCandleRaw, recvAt time.Time) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		return
	}
	bar := feedBarFromRaw(raw, feedBarSourceSocket, recvAt.UTC())
	if err := applySocketBar(st, bar); err != nil {
		o.logf("[feed] %s: dropped an invalid candle update: %v", key, err)
		return
	}
}

func (o *marketFeedOwner) IngestMids(mids map[string]float64, recvAt time.Time, source string) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	for coin, px := range mids {
		if !o.midCoins[coin] {
			continue
		}
		if !isFiniteFeedPrice(px) {
			continue
		}
		o.mids[coin] = feedMid{Px: px, RecvAt: recvAt.UTC(), Source: source}
	}
}

func (o *marketFeedOwner) MarkKeyRecovered(key marketFeedKey) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		return
	}
	if st.Status == feedStatusRepairing {
		st.Status = feedStatusReady
		st.StatusDetail = ""
	}
}

func (o *marketFeedOwner) keysAwaitingRepair() []marketFeedKey {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make([]marketFeedKey, 0, len(o.keys))
	for key, st := range o.keys {
		if st.Status == feedStatusRepairing {
			out = append(out, key)
		}
	}
	sortMarketFeedKeys(out)
	return out
}

func (o *marketFeedOwner) repairSpan(key marketFeedKey) (startMs int64, required int, intervalMs int64, ok bool) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		return 0, 0, 0, false
	}
	if len(st.Bars) == 0 {
		return 0, st.Required, st.IntervalMs, true
	}
	return st.Bars[len(st.Bars)-1].OpenMs, st.Required, st.IntervalMs, true
}

func (o *marketFeedOwner) fetchAndMerge(ctx context.Context, key marketFeedKey, reason feedRestReason) error {
	startMs, required, intervalMs, ok := o.repairSpan(key)
	if !ok {
		return fmt.Errorf("feed key %s is not tracked", key)
	}
	requestedAt := o.now()
	var raws []hlCandleRaw
	var err error
	if startMs == 0 {
		var hist hlCandleHistory
		hist, err = hlFetchCandleHistory(ctx, key.Symbol, key.Timeframe, required, requestedAt)
		raws = hist.Raws
	} else {
		endMs := requestedAt.UnixMilli()
		windowStart := startMs - intervalMs
		if windowStart < 0 {
			windowStart = 0
		}
		raws, err = fetchHyperliquidCandleSnapshotFn(ctx, key.Symbol, key.Timeframe, windowStart, endMs)
	}
	o.countRestCall(reason)
	if err != nil {
		o.feedMu.Lock()
		if st := o.keys[key]; st != nil {
			st.StatusDetail = err.Error()
			if len(st.Bars) == 0 {
				st.Status = feedStatusFailed
			}
		}
		o.feedMu.Unlock()
		return err
	}

	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		return fmt.Errorf("feed key %s is not tracked", key)
	}
	mergeRestRows(st, raws, requestedAt)
	drainPendingSocketBars(st)
	if len(st.Bars) == 0 {
		st.Status = feedStatusFailed
		st.StatusDetail = "the venue returned no candles"
		return fmt.Errorf("feed key %s: the venue returned no candles", key)
	}
	st.CoverageShort = len(st.Bars) < st.Required
	st.Status = feedStatusReady
	st.StatusDetail = ""
	if st.CoverageShort {
		st.StatusDetail = fmt.Sprintf("venue history is shorter than required (%d of %d bars)", len(st.Bars), st.Required)
	}
	return nil
}

func (o *marketFeedOwner) Readiness() []feedKeyReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	now := o.now()
	out := make([]feedKeyReadiness, 0, len(o.keys))
	for _, st := range o.keys {
		out = append(out, keyReadiness(st, now, o.connectedForReadinessLocked(now)))
	}
	sort.Slice(out, func(i, j int) bool { return marketFeedKeyLess(out[i].Key, out[j].Key) })
	return out
}

func (o *marketFeedOwner) connectedForReadinessLocked(now time.Time) bool {
	if !o.connected {
		return false
	}
	if o.lastConnectAt.IsZero() {
		return true
	}
	return now.Sub(o.lastConnectAt) > feedReconnectGrace
}

type marketFeedHealth struct {
	Mode         string                `json:"mode"`
	Connected    bool                  `json:"connected"`
	Generation   uint64                `json:"generation"`
	LastSnapshot string                `json:"last_snapshot,omitempty"`
	Metrics      feedMetrics           `json:"metrics"`
	Keys         []marketFeedHealthKey `json:"keys"`
	Mids         []marketFeedHealthMid `json:"mids,omitempty"`
}

type marketFeedHealthKey struct {
	Key           string `json:"key"`
	Status        string `json:"status"`
	Bars          int    `json:"bars"`
	Required      int    `json:"required"`
	Ready         bool   `json:"ready"`
	Stale         bool   `json:"stale,omitempty"`
	CoverageShort bool   `json:"coverage_short,omitempty"`
	Detail        string `json:"detail,omitempty"`
	LastCloseMs   int64  `json:"last_close_ms,omitempty"`
}

type marketFeedHealthMid struct {
	Coin     string  `json:"coin"`
	Px       float64 `json:"px"`
	AgeMs    int64   `json:"age_ms"`
	Source   string  `json:"source"`
	Stale    bool    `json:"stale,omitempty"`
	Complete bool    `json:"complete"`
}

func (o *marketFeedOwner) Health(lastSnapshotID string) marketFeedHealth {
	readiness := o.Readiness()
	o.feedMu.Lock()
	now := o.now()
	health := marketFeedHealth{
		Mode:         marketFeedWebsocket,
		Connected:    o.connected,
		Generation:   o.gen,
		LastSnapshot: lastSnapshotID,
		Metrics:      o.metrics,
	}
	coins := make([]string, 0, len(o.midCoins))
	for c := range o.midCoins {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		mid, ok := o.mids[coin]
		entry := marketFeedHealthMid{Coin: coin, Complete: ok}
		if ok {
			entry.Px = mid.Px
			entry.AgeMs = now.Sub(mid.RecvAt).Milliseconds()
			entry.Source = mid.Source
			entry.Stale = now.Sub(mid.RecvAt) > feedMidStaleAfter
		}
		health.Mids = append(health.Mids, entry)
	}
	o.feedMu.Unlock()

	for _, r := range readiness {
		health.Keys = append(health.Keys, marketFeedHealthKey{
			Key:           r.Key.String(),
			Status:        string(r.Status),
			Bars:          r.Bars,
			Required:      r.Required,
			Ready:         r.Ready,
			Stale:         r.Stale,
			CoverageShort: r.CoverageShort,
			Detail:        r.Detail,
			LastCloseMs:   r.LastCloseMs,
		})
	}
	return health
}

type marketFeedStatusHolder struct {
	statusMu   sync.Mutex
	owner      *marketFeedOwner
	snapshotID string
}

var globalMarketFeedStatus = &marketFeedStatusHolder{}

func (h *marketFeedStatusHolder) setOwner(owner *marketFeedOwner) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	h.owner = owner
}

func (h *marketFeedStatusHolder) setSnapshotID(id string) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	h.snapshotID = id
}

func (h *marketFeedStatusHolder) read() (*marketFeedOwner, string) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	return h.owner, h.snapshotID
}

func marketFeedStatusBlock() *marketFeedHealth {
	owner, snapshotID := globalMarketFeedStatus.read()
	if owner == nil {
		return &marketFeedHealth{Mode: marketFeedREST}
	}
	health := owner.Health(snapshotID)
	return &health
}

func formatFeedMetrics(m feedMetrics) string {
	return fmt.Sprintf("rest{bootstrap,repair,recovery}=%d,%d,%d steady_candle_rest=%d",
		m.BootstrapCalls, m.RepairCalls, m.RecoveryCalls, m.SteadyCandleCalls)
}

func feedKeySummary(readiness []feedKeyReadiness) string {
	parts := make([]string, 0, len(readiness))
	for _, r := range readiness {
		parts = append(parts, r.Key.PayloadID()+"="+string(r.Status))
	}
	return strings.Join(parts, " ")
}
