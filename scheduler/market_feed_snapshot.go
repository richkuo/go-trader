package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	marketSnapshotVersion = 1
	marketPayloadMaxBytes = 8 << 20
)

type marketFrame struct {
	Rows               []hlCandleRow `json:"rows"`
	Required           int           `json:"required"`
	Bars               int           `json:"bars"`
	CoverageShort      bool          `json:"coverage_short"`
	FirstOpenMs        int64         `json:"first_open_ms"`
	LastOpenMs         int64         `json:"last_open_ms"`
	LastCloseMs        int64         `json:"last_close_ms"`
	LastRecvAtMs       int64         `json:"last_recv_at_ms"`
	Source             string        `json:"source"`
	Ready              bool          `json:"ready"`
	Stale              bool          `json:"stale,omitempty"`
	StaleReason        string        `json:"stale_reason,omitempty"`
	FormingBarIncluded bool          `json:"forming_bar_included"`
}

type marketMidPayload struct {
	Px        float64 `json:"px"`
	RecvAtMs  int64   `json:"recv_at_ms"`
	Source    string  `json:"source"`
	AgeMs     int64   `json:"age_ms"`
	Stale     bool    `json:"stale,omitempty"`
	Confirmed bool    `json:"confirmed"`
}

type marketFundingPayload struct {
	Current     float64             `json:"current"`
	Avg7d       float64             `json:"avg_7d"`
	HasScalar   bool                `json:"has_scalar"`
	Records     []feedFundingRecord `json:"records,omitempty"`
	HasRecords  bool                `json:"has_records"`
	FetchedAtMs int64               `json:"fetched_at_ms"`
	Source      string              `json:"source"`
	Error       string              `json:"error,omitempty"`
}

type marketPayload struct {
	Version      int                             `json:"version"`
	SnapshotID   string                          `json:"snapshot_id"`
	Generation   uint64                          `json:"generation"`
	SealedAtMs   int64                           `json:"sealed_at_ms"`
	Frames       map[string]marketFrame          `json:"frames"`
	Mids         map[string]marketMidPayload     `json:"mids,omitempty"`
	Funding      map[string]marketFundingPayload `json:"funding,omitempty"`
	FeedComplete bool                            `json:"feed_complete"`
}

type marketSnapshotKey struct {
	Key        marketFeedKey
	IntervalMs int64
	Bars       []feedBar
	Readiness  feedKeyReadiness
}

type marketSnapshot struct {
	Version          int
	EvaluationID     string
	ConfigGeneration uint64
	SealedAt         time.Time
	Connected        bool
	Metrics          feedMetrics

	keys    map[marketFeedKey]*marketSnapshotKey
	mids    map[string]feedMid
	funding map[string]feedFunding
}

type cycleMarketRequirement struct {
	Key      marketFeedKey
	Required int
}

type cycleMarketRequirements struct {
	Keys    []cycleMarketRequirement
	Coins   []string
	Funding map[string]feedFundingNeed
}

func cycleRequirementsForDue(due []StrategyConfig, req feedRequirements) cycleMarketRequirements {
	need := make(map[marketFeedKey]int)
	coins := make(map[string]bool)
	funding := make(map[string]feedFundingNeed)
	raise := func(key marketFeedKey, required int) {
		if existing, ok := need[key]; !ok || required > existing {
			need[key] = required
		}
	}
	for _, sc := range due {
		entry, ok := req.Strategies[sc.ID]
		if !ok {
			continue
		}
		raise(entry.Signal, entry.SignalLookback)
		coins[entry.Coin] = true
		if entry.HasHTF {
			raise(entry.HTF, hlFeedHTFLookback)
		}
		if entry.HasRegime {
			raise(entry.Regime, entry.RegimeLookback)
		}
		if entry.FundingScalar || entry.FundingRecords {
			f := funding[entry.Coin]
			f.Scalar = f.Scalar || entry.FundingScalar
			f.Records = f.Records || entry.FundingRecords
			funding[entry.Coin] = f
		}
	}
	for _, coin := range req.MidCoins {
		coins[coin] = true
	}
	out := cycleMarketRequirements{Funding: funding}
	for key, required := range need {
		out.Keys = append(out.Keys, cycleMarketRequirement{Key: key, Required: required})
	}
	sort.Slice(out.Keys, func(i, j int) bool { return marketFeedKeyLess(out.Keys[i].Key, out.Keys[j].Key) })
	for coin := range coins {
		out.Coins = append(out.Coins, coin)
	}
	sort.Strings(out.Coins)
	return out
}

func (o *marketFeedOwner) publishedKey(key marketFeedKey) bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.published[key]
}

func (o *marketFeedOwner) recoverKeyOnce(ctx context.Context, key marketFeedKey) error {
	return o.fetchAndMerge(ctx, key, feedRestRecovery)
}

func (o *marketFeedOwner) earliestFrameBarMs(reqs cycleMarketRequirements) map[string]int64 {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make(map[string]int64)
	for _, cr := range reqs.Keys {
		st := o.keys[cr.Key]
		if st == nil || len(st.Bars) == 0 {
			continue
		}
		start := len(st.Bars) - cr.Required
		if start < 0 {
			start = 0
		}
		first := st.Bars[start].CloseMs
		if existing, ok := out[cr.Key.Symbol]; !ok || first < existing {
			out[cr.Key.Symbol] = first
		}
	}
	return out
}

func sealCycleMarketSnapshot(ctx context.Context, o *marketFeedOwner, reqs cycleMarketRequirements,
	evaluationID string, now time.Time) *marketSnapshot {
	if o == nil {
		return nil
	}
	for _, cr := range reqs.Keys {
		readiness, tracked := o.readinessFor(cr.Key)
		if !tracked {
			continue
		}
		if readiness.Ready && !readiness.Stale {
			continue
		}
		if err := o.recoverKeyOnce(ctx, cr.Key); err != nil {
			o.logf("[feed] %s: cycle recovery failed: %v", cr.Key, err)
			o.raiseAlert(cr.Key, "recovery_failed", err.Error())
		}
	}
	o.EnsureFunding(ctx, o.earliestFrameBarMs(reqs))

	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	snap := &marketSnapshot{
		Version:          marketSnapshotVersion,
		EvaluationID:     evaluationID,
		ConfigGeneration: o.gen,
		SealedAt:         now.UTC(),
		Connected:        o.connected,
		Metrics:          o.metrics,
		keys:             make(map[marketFeedKey]*marketSnapshotKey, len(reqs.Keys)),
		mids:             make(map[string]feedMid, len(reqs.Coins)),
		funding:          make(map[string]feedFunding),
	}
	connected := o.connectedForReadinessLocked(now)
	for _, cr := range reqs.Keys {
		st := o.keys[cr.Key]
		if st == nil || !o.published[cr.Key] {
			continue
		}
		frozen := &marketSnapshotKey{
			Key:        cr.Key,
			IntervalMs: st.IntervalMs,
			Bars:       append([]feedBar(nil), st.Bars...),
			Readiness:  keyReadiness(st, now, connected),
		}
		snap.keys[cr.Key] = frozen
	}
	for _, coin := range reqs.Coins {
		if mid, ok := o.mids[coin]; ok {
			snap.mids[coin] = mid
		}
	}
	for coin, need := range reqs.Funding {
		if !need.Scalar && !need.Records {
			continue
		}
		if f, ok := o.funding[coin]; ok && f != nil {
			cp := *f
			cp.Records = append([]feedFundingRecord(nil), f.Records...)
			snap.funding[coin] = cp
		}
	}
	return snap
}

func (o *marketFeedOwner) readinessFor(key marketFeedKey) (feedKeyReadiness, bool) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		return feedKeyReadiness{Key: key}, false
	}
	now := o.now()
	return keyReadiness(st, now, o.connectedForReadinessLocked(now)), true
}

func (s *marketSnapshot) age(now time.Time) time.Duration {
	if s == nil {
		return 0
	}
	return now.UTC().Sub(s.SealedAt)
}

func feedDecisionAgeLimit(intervalSeconds int) time.Duration {
	half := time.Duration(intervalSeconds) * time.Second / 2
	if half < feedDecisionAgeFloor {
		return feedDecisionAgeFloor
	}
	return half
}

func (s *marketSnapshot) frameFor(key marketFeedKey, required int) (marketFrame, bool) {
	if s == nil {
		return marketFrame{}, false
	}
	entry, ok := s.keys[key]
	if !ok || entry == nil {
		return marketFrame{}, false
	}
	if !entry.Readiness.Ready {
		return marketFrame{}, false
	}
	bars := entry.Bars
	if required > 0 && len(bars) > required {
		bars = bars[len(bars)-required:]
	}
	if len(bars) == 0 {
		return marketFrame{}, false
	}
	rows := make([]hlCandleRow, 0, len(bars))
	for _, b := range bars {
		rows = append(rows, b.row())
	}
	frame := marketFrame{
		Rows:               rows,
		Required:           required,
		Bars:               len(bars),
		CoverageShort:      len(bars) < required,
		FirstOpenMs:        bars[0].OpenMs,
		LastOpenMs:         bars[len(bars)-1].OpenMs,
		LastCloseMs:        bars[len(bars)-1].CloseMs,
		Source:             entry.Readiness.Source,
		Ready:              entry.Readiness.Ready,
		Stale:              entry.Readiness.Stale,
		StaleReason:        entry.Readiness.StaleReason,
		FormingBarIncluded: true,
	}
	if !entry.Readiness.LastRecvAt.IsZero() {
		frame.LastRecvAtMs = entry.Readiness.LastRecvAt.UTC().UnixMilli()
	}
	return frame, true
}

func (s *marketSnapshot) keyFailed(key marketFeedKey) bool {
	if s == nil {
		return false
	}
	entry, ok := s.keys[key]
	return !ok || entry == nil || !entry.Readiness.Ready
}

func (s *marketSnapshot) midFor(coin string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	mid, ok := s.mids[coin]
	if !ok || mid.Px <= 0 {
		return 0, false
	}
	if s.SealedAt.Sub(mid.RecvAt) > feedMidStaleAfter {
		return 0, false
	}
	return mid.Px, true
}

func (s *marketSnapshot) freshMids() map[string]float64 {
	out := make(map[string]float64)
	if s == nil {
		return out
	}
	for coin := range s.mids {
		if px, ok := s.midFor(coin); ok {
			out[coin] = px
		}
	}
	return out
}

type marketPayloadFrameSpec struct {
	Key      marketFeedKey
	Required int
}

func marketPayloadFor(s *marketSnapshot, frames []marketPayloadFrameSpec, coins []string) (*marketPayload, error) {
	if s == nil {
		return nil, fmt.Errorf("no sealed market snapshot")
	}
	payload := &marketPayload{
		Version:      s.Version,
		SnapshotID:   s.EvaluationID,
		Generation:   s.ConfigGeneration,
		SealedAtMs:   s.SealedAt.UnixMilli(),
		Frames:       make(map[string]marketFrame, len(frames)),
		FeedComplete: true,
	}
	for _, spec := range frames {
		frame, ok := s.frameFor(spec.Key, spec.Required)
		if !ok {
			return nil, fmt.Errorf("sealed snapshot has no ready frame for %s", spec.Key)
		}
		payload.Frames[spec.Key.PayloadID()] = frame
	}
	for _, coin := range coins {
		mid, ok := s.mids[coin]
		if !ok {
			continue
		}
		age := s.SealedAt.Sub(mid.RecvAt)
		if payload.Mids == nil {
			payload.Mids = make(map[string]marketMidPayload, len(coins))
		}
		payload.Mids[coin] = marketMidPayload{
			Px:        mid.Px,
			RecvAtMs:  mid.RecvAt.UnixMilli(),
			Source:    mid.Source,
			AgeMs:     age.Milliseconds(),
			Stale:     age > feedMidStaleAfter,
			Confirmed: mid.Px > 0,
		}
	}
	for _, coin := range coins {
		f, ok := s.funding[coin]
		if !ok {
			continue
		}
		if payload.Funding == nil {
			payload.Funding = make(map[string]marketFundingPayload, len(coins))
		}
		payload.Funding[coin] = marketFundingPayload{
			Current:     f.Current,
			Avg7d:       f.Avg7d,
			HasScalar:   f.HasScalar,
			Records:     f.Records,
			HasRecords:  f.HasRecords,
			FetchedAtMs: f.FetchedAt.UnixMilli(),
			Source:      f.Source,
			Error:       f.Err,
		}
	}
	return payload, nil
}

func (s *marketSnapshot) fundingHold(coin string, scalar, records bool) (bool, string) {
	if !scalar && !records {
		return false, ""
	}
	if s == nil {
		return true, fmt.Sprintf("no sealed funding for %s", coin)
	}
	f, ok := s.funding[coin]
	switch {
	case !ok:
		return true, fmt.Sprintf("no sealed funding for %s", coin)
	case f.Err != "":
		return true, fmt.Sprintf("funding fetch for %s failed: %s", coin, f.Err)
	case scalar && !f.HasScalar:
		return true, fmt.Sprintf("sealed funding for %s carries no current rate", coin)
	case records && !f.HasRecords:
		return true, fmt.Sprintf("sealed funding for %s carries no history", coin)
	}
	return false, ""
}

type marketStdinEnvelope struct {
	Version int            `json:"v"`
	Market  *marketPayload `json:"market"`
}

func marketStdinJSON(payload *marketPayload) ([]byte, error) {
	if payload == nil {
		return nil, fmt.Errorf("no market payload to send")
	}
	blob, err := json.Marshal(marketStdinEnvelope{Version: hlBatchProtocolVersionMarket, Market: payload})
	if err != nil {
		return nil, fmt.Errorf("marshal market stdin envelope: %w", err)
	}
	if len(blob) > marketPayloadMaxBytes {
		return nil, fmt.Errorf("market payload is %d bytes, over the %d-byte cap", len(blob), marketPayloadMaxBytes)
	}
	return blob, nil
}

func marketPayloadJSON(payload *marketPayload) ([]byte, error) {
	blob, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal market payload: %w", err)
	}
	if len(blob) > marketPayloadMaxBytes {
		return nil, fmt.Errorf("market payload is %d bytes, over the %d-byte cap", len(blob), marketPayloadMaxBytes)
	}
	return blob, nil
}

func marketSnapshotLogLine(s *marketSnapshot, reqs cycleMarketRequirements) string {
	if s == nil {
		return ""
	}
	ready, stale := 0, 0
	for _, entry := range s.keys {
		if entry.Readiness.Ready {
			ready++
		}
		if entry.Readiness.Stale {
			stale++
		}
	}
	return fmt.Sprintf("[feed] snapshot=%s/%d keys=%d ready=%d stale=%d %s",
		s.EvaluationID, s.ConfigGeneration, len(reqs.Keys), ready, stale, formatFeedMetrics(s.Metrics))
}
