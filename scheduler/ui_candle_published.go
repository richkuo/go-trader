package main

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	publishedCandleMaxIdentities = 512
	publishedCandleMaxBars       = 5000

	publishedProvenancePrimary  = "primary"
	publishedProvenanceBackup   = "backup"
	publishedProvenanceEmbedded = "embedded"
)

type publishedCandleStatus struct {
	Ready      bool      `json:"ready"`
	Status     string    `json:"status,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Evaluation string    `json:"evaluation_id,omitempty"`
}

type publishedCandleSeries struct {
	Identity          candleIdentity
	IntervalMs        int64
	Bars              []UICandle
	LastCloseMs       int64
	FormingOpen       int64
	FeedGeneration    uint64
	EvaluationID      string
	SealedAt          time.Time
	Required          int
	CoverageShort     bool
	SealStale         bool
	SealStaleReason   string
	Provenance        string
	FormingBarOmitted bool
	Latest            publishedCandleStatus
	touched           time.Time
}

type publishedCandleStore struct {
	mu      sync.Mutex
	entries map[candleIdentity]*publishedCandleSeries
	epoch   atomic.Uint64
	limit   int
}

func newPublishedCandleStore() *publishedCandleStore {
	return &publishedCandleStore{entries: make(map[candleIdentity]*publishedCandleSeries), limit: publishedCandleMaxIdentities}
}

type publishedCandleUpdate struct {
	id     candleIdentity
	series *publishedCandleSeries
	status publishedCandleStatus
}

func publishedProvenance(entry *marketSnapshotKey, shared bool) string {
	switch {
	case !shared:
		return publishedProvenanceEmbedded
	case entry.FilledFrom != "":
		return publishedProvenanceBackup
	default:
		return publishedProvenancePrimary
	}
}

func freezePublishedSeries(snap *marketSnapshot, id candleIdentity, entry *marketSnapshotKey, shared bool) *publishedCandleSeries {
	bars := entry.Bars
	if len(bars) > publishedCandleMaxBars {
		bars = bars[len(bars)-publishedCandleMaxBars:]
	}
	out := make([]UICandle, 0, len(bars))
	var lastClose int64
	prev := int64(-1)
	for _, b := range bars {
		if b.OpenMs%1000 != 0 || b.OpenMs <= prev {
			return nil
		}
		prev = b.OpenMs
		out = append(out, UICandle{Time: b.OpenMs / 1000, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
		lastClose = b.CloseMs
	}
	if len(out) == 0 {
		return nil
	}
	s := &publishedCandleSeries{
		Identity:          id,
		IntervalMs:        entry.IntervalMs,
		Bars:              out,
		LastCloseMs:       lastClose,
		FeedGeneration:    snap.ConfigGeneration,
		EvaluationID:      snap.EvaluationID,
		SealedAt:          snap.SealedAt.UTC(),
		Required:          entry.Readiness.Required,
		CoverageShort:     entry.Readiness.CoverageShort,
		SealStale:         entry.Readiness.Stale,
		SealStaleReason:   entry.Readiness.StaleReason,
		Provenance:        publishedProvenance(entry, shared),
		FormingBarOmitted: entry.FormingBarOmitted,
	}
	last := bars[len(bars)-1]
	if !entry.FormingBarOmitted && entry.IntervalMs > 0 && last.OpenMs+entry.IntervalMs > snap.SealedAt.UTC().UnixMilli() {
		s.FormingOpen = last.OpenMs / 1000
	}
	return s
}

func (p *publishedCandleStore) Publish(snap *marketSnapshot, reqs cycleMarketRequirements, shared bool) {
	if p == nil || snap == nil {
		return
	}
	observed := time.Now().UTC()
	var updates []publishedCandleUpdate
	seen := make(map[marketFeedKey]bool, len(snap.keys))
	for key, entry := range snap.keys {
		seen[key] = true
		id, ok := candleIdentityForFeedKey(key)
		if !ok || entry == nil {
			continue
		}
		st := publishedCandleStatus{Ready: entry.Readiness.Ready, Status: string(entry.Readiness.Status), Detail: entry.Readiness.Detail,
			ObservedAt: observed, Evaluation: snap.EvaluationID}
		u := publishedCandleUpdate{id: id, status: st}
		if entry.Readiness.Ready {
			u.series = freezePublishedSeries(snap, id, entry, shared)
			if u.series == nil {
				u.status.Ready = false
				u.status.Status = "invalid"
				u.status.Detail = "the sealed bars are not on whole-second ascending opening times"
			}
		}
		updates = append(updates, u)
	}
	for _, cr := range reqs.Keys {
		if seen[cr.Key] {
			continue
		}
		id, ok := candleIdentityForFeedKey(cr.Key)
		if !ok {
			continue
		}
		updates = append(updates, publishedCandleUpdate{id: id, status: publishedCandleStatus{Status: "unavailable",
			Detail: "the cycle asked for this key and the snapshot does not hold it", ObservedAt: observed, Evaluation: snap.EvaluationID}})
	}
	if len(updates) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for _, u := range updates {
		cur := p.entries[u.id]
		if u.series != nil {
			u.series.Latest = u.status
			u.series.touched = observed
			p.entries[u.id] = u.series
			changed = true
			continue
		}
		if cur == nil {
			p.entries[u.id] = &publishedCandleSeries{Identity: u.id, Latest: u.status, touched: observed}
			continue
		}
		next := *cur
		next.Latest = u.status
		next.touched = observed
		p.entries[u.id] = &next
	}
	p.evictLocked()
	if changed {
		p.epoch.Add(1)
	}
}

func (p *publishedCandleStore) evictLocked() {
	if p.limit <= 0 || len(p.entries) <= p.limit {
		return
	}
	ids := make([]candleIdentity, 0, len(p.entries))
	for id := range p.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := p.entries[ids[i]], p.entries[ids[j]]
		if !a.touched.Equal(b.touched) {
			return a.touched.Before(b.touched)
		}
		return ids[i].String() < ids[j].String()
	})
	for _, id := range ids {
		if len(p.entries) <= p.limit {
			return
		}
		delete(p.entries, id)
	}
}

func (p *publishedCandleStore) get(id candleIdentity) *publishedCandleSeries {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[id]
}

func (p *publishedCandleStore) timeframes(series candleIdentity) []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for id, s := range p.entries {
		if id.series() == series && len(s.Bars) > 0 {
			out = append(out, id.Timeframe)
		}
	}
	sortCandleTimeframes(out)
	return out
}

func (p *publishedCandleStore) count() (identities, ready int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.entries {
		identities++
		if len(s.Bars) > 0 {
			ready++
		}
	}
	return identities, ready
}

func (s *publishedCandleSeries) stale(now time.Time) (bool, string) {
	if s == nil || len(s.Bars) == 0 {
		return true, "no published bars"
	}
	if s.SealStale {
		return true, s.SealStaleReason
	}
	nowMs := now.UTC().UnixMilli()
	if s.IntervalMs > 0 && nowMs-s.LastCloseMs > 2*s.IntervalMs+int64(feedStaleGrace/time.Millisecond) {
		return true, "the newest published bar is older than two intervals"
	}
	return false, ""
}

func (s *publishedCandleSeries) closedBars() []UICandle {
	if s.FormingOpen != 0 && len(s.Bars) > 0 && s.Bars[len(s.Bars)-1].Time == s.FormingOpen {
		return s.Bars[:len(s.Bars)-1]
	}
	return s.Bars
}
