package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	feedObservationOpenInterest = "open_interest"
	feedObservationUnitsBase    = "base"
	feedObservationTimeReceipt  = "receipt"
	feedObservationSourceHLWS   = "hyperliquid_ws_activeAssetCtx"
	feedObservationCadenceMs    = int64(60_000)
	feedObservationStaleAfterMs = 2 * feedObservationCadenceMs
	feedObservationMaxWindowMs  = int64(30 * 24 * 3_600_000)
	feedObservationRetainSlack  = 2 * feedObservationCadenceMs

	feedObservationGapDisconnected = "disconnected"

	feedObservationStatusWarming = "warming"
	feedObservationStatusReady   = "ready"
	feedObservationStatusStale   = "stale"
	feedObservationStatusGap     = "gap"
	feedObservationStatusEmpty   = "no_samples"

	openInterestBreakoutStrategyName = "open_interest_breakout"
	openInterestDefaultLookbackBars  = 4
	openInterestDefaultMaxAgeMs      = int64(120_000)
)

type feedObservationKey struct {
	Host      string
	Namespace string
	Coin      string
	Kind      string
}

func (k feedObservationKey) String() string {
	return k.Host + "|" + k.Namespace + "|" + k.Coin + "|" + k.Kind
}

func (k feedObservationKey) PayloadID() string {
	return k.Coin + "|" + k.Kind
}

func feedObservationKeyLess(a, b feedObservationKey) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Coin != b.Coin {
		return a.Coin < b.Coin
	}
	return a.Kind < b.Kind
}

func sortFeedObservationKeys(keys []feedObservationKey) {
	sort.Slice(keys, func(i, j int) bool { return feedObservationKeyLess(keys[i], keys[j]) })
}

func openInterestKeyFor(coin string) feedObservationKey {
	return feedObservationKey{Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, Kind: feedObservationOpenInterest}
}

type feedObservationSample struct {
	RecvAtMs  int64
	EventAtMs int64
	Value     float64
	Session   uint64
	Seq       uint64
}

type feedObservationGap struct {
	StartMs    int64
	EndMs      int64
	DetectedMs int64
	Reason     string
}

type feedObservationStats struct {
	Received   uint64
	Accepted   uint64
	Refreshed  uint64
	Rejected   uint64
	LastReject string
}

type feedObservationState struct {
	Key          feedObservationKey
	Source       string
	Units        string
	TimeBasis    string
	CadenceMs    int64
	WindowMs     int64
	Samples      []feedObservationSample
	Gaps         []feedObservationGap
	LastRecvAtMs int64
	Stats        feedObservationStats
	seq          uint64
}

func newFeedObservationState(key feedObservationKey, windowMs int64) *feedObservationState {
	return &feedObservationState{
		Key:       key,
		Source:    feedObservationSourceHLWS,
		Units:     feedObservationUnitsBase,
		TimeBasis: feedObservationTimeReceipt,
		CadenceMs: feedObservationCadenceMs,
		WindowMs:  windowMs,
	}
}

func (s *feedObservationState) raiseWindow(windowMs int64) {
	if windowMs > s.WindowMs {
		s.WindowMs = windowMs
	}
}

func (s *feedObservationState) retainMs() int64 {
	return s.WindowMs + feedObservationRetainSlack
}

func feedObservationBucket(recvMs, cadenceMs int64) int64 {
	if cadenceMs <= 0 {
		return recvMs
	}
	q := recvMs / cadenceMs
	if recvMs%cadenceMs != 0 {
		q++
	}
	return q * cadenceMs
}

func (s *feedObservationState) openGap() *feedObservationGap {
	if n := len(s.Gaps); n > 0 && s.Gaps[n-1].EndMs == 0 {
		return &s.Gaps[n-1]
	}
	return nil
}

func (s *feedObservationState) markDisconnected(atMs int64) {
	if s.LastRecvAtMs == 0 || s.openGap() != nil {
		return
	}
	start := s.LastRecvAtMs
	if atMs > 0 && atMs < start {
		start = atMs
	}
	detected := atMs
	if detected < start {
		detected = start
	}
	s.Gaps = append(s.Gaps, feedObservationGap{StartMs: start, DetectedMs: detected, Reason: feedObservationGapDisconnected})
}

type feedObservationOutcome struct {
	Accepted  bool
	Refreshed bool
	Reason    string
	Sample    feedObservationSample
}

func (s *feedObservationState) ingest(value float64, eventAtMs, recvMs int64, session uint64) feedObservationOutcome {
	s.Stats.Received++
	reject := func(reason string) feedObservationOutcome {
		s.Stats.Rejected++
		s.Stats.LastReject = reason
		return feedObservationOutcome{Reason: reason}
	}
	switch {
	case math.IsNaN(value) || math.IsInf(value, 0):
		return reject("nonfinite_value")
	case value < 0:
		return reject("negative_value")
	case recvMs <= 0:
		return reject("no_receipt_time")
	case recvMs < s.LastRecvAtMs:
		return reject(fmt.Sprintf("clock_regression: receipt %d is before the previous receipt %d", recvMs, s.LastRecvAtMs))
	}
	if gap := s.openGap(); gap != nil {
		gap.EndMs = recvMs
	}
	s.seq++
	sample := feedObservationSample{RecvAtMs: recvMs, EventAtMs: eventAtMs, Value: value, Session: session, Seq: s.seq}
	out := feedObservationOutcome{Accepted: true, Sample: sample}
	if n := len(s.Samples); n > 0 && feedObservationBucket(s.Samples[n-1].RecvAtMs, s.CadenceMs) == feedObservationBucket(recvMs, s.CadenceMs) {
		s.Samples[n-1] = sample
		s.Stats.Refreshed++
		out.Refreshed = true
	} else {
		s.Samples = append(s.Samples, sample)
		s.Stats.Accepted++
	}
	s.LastRecvAtMs = recvMs
	s.trim(recvMs)
	return out
}

func (s *feedObservationState) trim(nowMs int64) {
	floor := nowMs - s.retainMs()
	cut := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].RecvAtMs >= floor })
	if cut > 0 {
		s.Samples = append([]feedObservationSample(nil), s.Samples[cut:]...)
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

type feedObservationReadiness struct {
	Key          feedObservationKey
	Status       string
	Detail       string
	Ready        bool
	Samples      int
	FirstRecvMs  int64
	LastRecvMs   int64
	CoveredMs    int64
	WindowMs     int64
	OpenGap      bool
	Stats        feedObservationStats
	SourceDetail string
}

func observationReadiness(s *feedObservationState, nowMs int64) feedObservationReadiness {
	out := feedObservationReadiness{
		Key:          s.Key,
		Samples:      len(s.Samples),
		WindowMs:     s.WindowMs,
		Stats:        s.Stats,
		SourceDetail: s.Source + "/" + s.Units + "/" + s.TimeBasis,
	}
	if len(s.Samples) == 0 {
		out.Status = feedObservationStatusEmpty
		out.Detail = "no accepted samples since this owner started"
		return out
	}
	out.FirstRecvMs = s.Samples[0].RecvAtMs
	out.LastRecvMs = s.Samples[len(s.Samples)-1].RecvAtMs
	start := out.FirstRecvMs
	for _, g := range s.Gaps {
		if g.EndMs != 0 && g.EndMs > start {
			start = g.EndMs
		}
	}
	out.CoveredMs = out.LastRecvMs - start
	if gap := s.openGap(); gap != nil {
		out.OpenGap = true
		out.Status = feedObservationStatusGap
		out.Detail = fmt.Sprintf("%s since %d", gap.Reason, gap.StartMs)
		return out
	}
	if age := nowMs - out.LastRecvMs; age > feedObservationStaleAfterMs {
		out.Status = feedObservationStatusStale
		out.Detail = fmt.Sprintf("newest sample is %dms old, over %dms", age, feedObservationStaleAfterMs)
		return out
	}
	if out.CoveredMs < s.WindowMs {
		out.Status = feedObservationStatusWarming
		out.Detail = fmt.Sprintf("%dms of continuous samples, %dms required", out.CoveredMs, s.WindowMs)
		return out
	}
	out.Status = feedObservationStatusReady
	out.Ready = true
	return out
}

func openInterestParamInt(params map[string]interface{}, name string, def int64) (int64, error) {
	raw, ok := params[name]
	if !ok || raw == nil {
		return def, nil
	}
	var v float64
	switch t := raw.(type) {
	case float64:
		v = t
	case int:
		v = float64(t)
	case int64:
		v = float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s %q is not a number", name, t.String())
		}
		v = f
	default:
		return 0, fmt.Errorf("%s must be a number, got %v", name, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %v", name, raw)
	}
	return int64(v), nil
}

func openInterestWindowMs(sc StrategyConfig, intervalMs int64) (int64, error) {
	params := sc.OpenStrategy.Params
	lookback, err := openInterestParamInt(params, "oi_lookback", openInterestDefaultLookbackBars)
	if err != nil {
		return 0, err
	}
	maxAge, err := openInterestParamInt(params, "max_observation_age_ms", openInterestDefaultMaxAgeMs)
	if err != nil {
		return 0, err
	}
	window := lookback*intervalMs + maxAge + feedObservationCadenceMs
	if window > feedObservationMaxWindowMs {
		return 0, fmt.Errorf("open-interest window %dms (oi_lookback %d bars, max_observation_age_ms %d) is over the %dms retention cap",
			window, lookback, maxAge, feedObservationMaxWindowMs)
	}
	return window, nil
}

func feedStrategyUsesOpenInterest(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == openInterestBreakoutStrategyName
}

type feedSocketAssetCtx struct {
	Coin string                     `json:"coin"`
	Ctx  map[string]json.RawMessage `json:"ctx"`
}

func parseFeedOpenInterest(data json.RawMessage) (string, float64, error) {
	var msg feedSocketAssetCtx
	if err := json.Unmarshal(data, &msg); err != nil {
		return "", 0, fmt.Errorf("decode activeAssetCtx: %w", err)
	}
	coin := strings.TrimSpace(msg.Coin)
	if coin == "" {
		return "", 0, fmt.Errorf("activeAssetCtx carries no coin")
	}
	raw, ok := msg.Ctx["openInterest"]
	if !ok {
		return coin, 0, fmt.Errorf("activeAssetCtx for %s carries no openInterest", coin)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return coin, 0, fmt.Errorf("activeAssetCtx openInterest for %s is not a decimal string: %s", coin, string(raw))
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return coin, 0, fmt.Errorf("activeAssetCtx openInterest for %s: %w", coin, err)
	}
	return coin, v, nil
}

func (o *marketFeedOwner) IngestOpenInterest(coin string, value float64, recvAt time.Time, raw []byte, parseErr error) {
	recvMs := recvAt.UTC().UnixMilli()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	key := openInterestKeyFor(coin)
	st := o.observations[key]
	if st == nil {
		return
	}
	var out feedObservationOutcome
	if parseErr != nil {
		st.Stats.Received++
		st.Stats.Rejected++
		st.Stats.LastReject = parseErr.Error()
		out = feedObservationOutcome{Reason: parseErr.Error()}
	} else {
		out = st.ingest(value, 0, recvMs, o.obsSession)
	}
	if o.recorder != nil {
		o.recorder.offer(observationRecord{
			Kind: "obs", Coin: coin, ObsKind: feedObservationOpenInterest, RecvMs: recvMs,
			Session: o.obsSession, Raw: string(raw), Value: value, Accepted: out.Accepted,
			Refreshed: out.Refreshed, Reason: out.Reason, Seq: out.Sample.Seq,
		})
	}
	if !out.Accepted {
		o.logf("[feed] %s: rejected an open-interest record: %s", key.PayloadID(), out.Reason)
	}
}

func (o *marketFeedOwner) markObservationsDisconnectedLocked(atMs int64) {
	for _, st := range o.observations {
		st.markDisconnected(atMs)
	}
}

func (o *marketFeedOwner) ObservationCoins() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	set := make(map[string]bool, len(o.observations))
	for k := range o.observations {
		if k.Kind == feedObservationOpenInterest {
			set[k.Coin] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) ObservationReadiness() []feedObservationReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	out := make([]feedObservationReadiness, 0, len(o.observations))
	for _, st := range o.observations {
		out = append(out, observationReadiness(st, nowMs))
	}
	sort.Slice(out, func(i, j int) bool { return feedObservationKeyLess(out[i].Key, out[j].Key) })
	return out
}

func (o *marketFeedOwner) applyObservationNeedsLocked(needs map[feedObservationKey]int64) {
	for key, window := range needs {
		if st := o.observations[key]; st != nil {
			st.raiseWindow(window)
			continue
		}
		o.observations[key] = newFeedObservationState(key, window)
	}
}

func (o *marketFeedOwner) dropUnneededObservationsLocked(needs map[feedObservationKey]int64) {
	for key := range o.observations {
		if _, ok := needs[key]; !ok {
			delete(o.observations, key)
		}
	}
}

type marketSnapshotObservation struct {
	Key       feedObservationKey
	Source    string
	Units     string
	TimeBasis string
	CadenceMs int64
	WindowMs  int64
	CutoffMs  int64
	Samples   []feedObservationSample
	Gaps      []feedObservationGap
	Readiness feedObservationReadiness
}

func freezeObservation(st *feedObservationState, windowMs, cutoffMs, nowMs int64) *marketSnapshotObservation {
	start := cutoffMs - windowMs
	out := &marketSnapshotObservation{
		Key:       st.Key,
		Source:    st.Source,
		Units:     st.Units,
		TimeBasis: st.TimeBasis,
		CadenceMs: st.CadenceMs,
		WindowMs:  windowMs,
		CutoffMs:  cutoffMs,
		Samples:   []feedObservationSample{},
		Gaps:      []feedObservationGap{},
		Readiness: observationReadiness(st, nowMs),
	}
	for _, s := range st.Samples {
		if s.RecvAtMs > cutoffMs || s.RecvAtMs < start {
			continue
		}
		out.Samples = append(out.Samples, s)
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

type marketObservationSample struct {
	RecvAtMs  int64   `json:"recv_ms"`
	EventAtMs *int64  `json:"event_ms"`
	Value     float64 `json:"value"`
	Session   uint64  `json:"session"`
	Seq       uint64  `json:"seq"`
}

type marketObservationGap struct {
	StartMs    int64  `json:"start_ms"`
	EndMs      *int64 `json:"end_ms"`
	DetectedMs int64  `json:"detected_ms"`
	Reason     string `json:"reason"`
}

type marketObservationPayload struct {
	Kind          string                    `json:"kind"`
	Coin          string                    `json:"coin"`
	Source        string                    `json:"source"`
	Units         string                    `json:"units"`
	TimeBasis     string                    `json:"time_basis"`
	CadenceMs     int64                     `json:"cadence_ms"`
	WindowMs      int64                     `json:"window_ms"`
	CutoffMs      int64                     `json:"cutoff_ms"`
	BarIntervalMs int64                     `json:"bar_interval_ms"`
	BarOffsetMs   int64                     `json:"bar_endpoint_offset_ms"`
	SnapshotID    string                    `json:"snapshot_id"`
	Available     bool                      `json:"available"`
	Reason        string                    `json:"reason,omitempty"`
	FeedStatus    string                    `json:"feed_status,omitempty"`
	Samples       []marketObservationSample `json:"samples"`
	Gaps          []marketObservationGap    `json:"gaps"`
}

func (s *marketSnapshot) observationPayload(key feedObservationKey, windowMs, barIntervalMs int64) marketObservationPayload {
	out := marketObservationPayload{
		Kind:          key.Kind,
		Coin:          key.Coin,
		Source:        feedObservationSourceHLWS,
		Units:         feedObservationUnitsBase,
		TimeBasis:     feedObservationTimeReceipt,
		CadenceMs:     feedObservationCadenceMs,
		WindowMs:      windowMs,
		BarIntervalMs: barIntervalMs,
		BarOffsetMs:   1,
		Samples:       []marketObservationSample{},
		Gaps:          []marketObservationGap{},
	}
	if s == nil {
		out.Reason = "no sealed market snapshot"
		return out
	}
	out.SnapshotID = s.EvaluationID
	entry, ok := s.observations[key]
	if !ok || entry == nil {
		out.Reason = fmt.Sprintf("%s is not in the sealed snapshot", key.PayloadID())
		out.CutoffMs = s.observationCutoff().UnixMilli()
		return out
	}
	out.Source, out.Units, out.TimeBasis, out.CadenceMs = entry.Source, entry.Units, entry.TimeBasis, entry.CadenceMs
	out.CutoffMs = entry.CutoffMs
	out.FeedStatus = entry.Readiness.Status
	if entry.WindowMs < windowMs {
		out.Reason = fmt.Sprintf("%s window %dms in the seal is below the %dms this strategy needs", key.PayloadID(), entry.WindowMs, windowMs)
		return out
	}
	start := entry.CutoffMs - windowMs
	for _, smp := range entry.Samples {
		if smp.RecvAtMs < start || smp.RecvAtMs > entry.CutoffMs {
			continue
		}
		ms := marketObservationSample{RecvAtMs: smp.RecvAtMs, Value: smp.Value, Session: smp.Session, Seq: smp.Seq}
		if smp.EventAtMs != 0 {
			ev := smp.EventAtMs
			ms.EventAtMs = &ev
		}
		out.Samples = append(out.Samples, ms)
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

func (s *marketSnapshot) observationCutoff() time.Time {
	if s == nil {
		return time.Time{}
	}
	if !s.Deadline.IsZero() {
		return s.Deadline
	}
	return s.SealedAt
}

func attachObservationPayloads(payload *marketPayload, s *marketSnapshot, needs map[feedObservationKey]int64, barIntervalMs int64) {
	if payload == nil || len(needs) == 0 {
		return
	}
	keys := make([]feedObservationKey, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	sortFeedObservationKeys(keys)
	payload.Observations = make(map[string]marketObservationPayload, len(keys))
	for _, k := range keys {
		payload.Observations[k.PayloadID()] = s.observationPayload(k, needs[k], barIntervalMs)
	}
}
