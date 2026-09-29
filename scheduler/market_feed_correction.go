package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	feedCorrectionTick        = time.Second
	feedCorrectionCoalesce    = 5 * time.Second
	feedCorrectionReadTimeout = 10 * time.Second
	feedCorrectionOverdue     = 60 * time.Second
	feedCorrectionGiveUp      = 10 * time.Minute
	feedCorrectionRetryMin    = 2 * time.Second
	feedCorrectionRetryMax    = 30 * time.Second
	feedCorrectionParallel    = 4
)

var feedCorrectionOffsets = []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}

type feedKeyCorrection struct {
	started     bool
	inflight    bool
	retryAt     time.Time
	failures    int
	lastErr     string
	finalized   int64
	overdue     bool
	pendingBars int
	overdueBars int
}

type feedCorrectionStats struct {
	Reads          int    `json:"reads"`
	Retries        int    `json:"retries"`
	Failed         int    `json:"failed"`
	Refused        int    `json:"refused"`
	Corrected      int    `json:"corrected"`
	Added          int    `json:"added"`
	RestOlder      int    `json:"rest_older"`
	Unverified     int    `json:"unverified"`
	LastCorrection string `json:"last_correction,omitempty"`
}

type feedCorrectionHealth struct {
	Offsets []string `json:"offsets"`
	Pending int      `json:"pending"`
	Overdue int      `json:"overdue"`
	feedCorrectionStats
}

type feedCorrectionJob struct {
	key        marketFeedKey
	st         *feedKeyState
	fromOpenMs int64
	retry      bool
}

func feedBarCloseAt(b feedBar, intervalMs int64) time.Time {
	return time.UnixMilli(b.OpenMs + intervalMs).UTC()
}

func feedBarSameValues(a, b feedBar) bool {
	return a.CloseMs == b.CloseMs && a.HasClose == b.HasClose && a.Open == b.Open && a.High == b.High &&
		a.Low == b.Low && a.Close == b.Close && a.Volume == b.Volume
}

func feedRestBarDecision(stored, rest feedBar, requestedAt time.Time) (replace, verified bool) {
	if rest.Volume < stored.Volume {
		return false, stored.RecvAt.After(requestedAt)
	}
	if rest.Volume > stored.Volume {
		return true, true
	}
	if stored.RecvAt.After(requestedAt) {
		return false, true
	}
	return true, true
}

func feedBarChangedFields(prev, next feedBar) string {
	var parts []string
	add := func(name string, a, b float64) {
		if a != b {
			parts = append(parts, fmt.Sprintf("%s:%s->%s", name, strconv.FormatFloat(a, 'g', -1, 64), strconv.FormatFloat(b, 'g', -1, 64)))
		}
	}
	add("open", prev.Open, next.Open)
	add("high", prev.High, next.High)
	add("low", prev.Low, next.Low)
	add("close", prev.Close, next.Close)
	add("volume", prev.Volume, next.Volume)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

type feedBarChange struct {
	OpenMs      int64
	Added       bool
	Fields      string
	PriorSource feedBarSource
}

type feedCorrectionOutcome struct {
	Confirmed int
	Corrected int
	Added     int
	KeptNewer int
	RestOlder int
	Invalid   int
	Changes   []feedBarChange
}

func mergeCorrectionRows(s *feedKeyState, raws []hlCandleRaw, requestedAt time.Time) feedCorrectionOutcome {
	var out feedCorrectionOutcome
	reqMs := requestedAt.UnixMilli()
	for _, raw := range raws {
		bar := feedBarFromRaw(raw, s.IntervalMs, feedBarSourceREST, requestedAt)
		if err := validateFeedBar(bar, s.IntervalMs); err != nil {
			s.recordInvalid(err)
			out.Invalid++
			continue
		}
		if bar.OpenMs+s.IntervalMs > reqMs {
			continue
		}
		bar.RestSeenAt = requestedAt
		idx, found := s.indexOfOpen(bar.OpenMs)
		if !found {
			if len(s.Bars) >= s.Capacity && idx == 0 {
				continue
			}
			bar.Seq = s.nextSeq()
			s.insertAt(idx, bar)
			out.Added++
			out.Changes = append(out.Changes, feedBarChange{OpenMs: bar.OpenMs, Added: true, Fields: "missing bar added"})
			continue
		}
		stored := s.Bars[idx]
		replace, verified := feedRestBarDecision(stored, bar, requestedAt)
		if !replace || feedBarSameValues(stored, bar) {
			if !verified {
				out.RestOlder++
				continue
			}
			if requestedAt.After(stored.RestSeenAt) {
				s.Bars[idx].RestSeenAt = requestedAt
			}
			if replace {
				out.Confirmed++
			} else {
				out.KeptNewer++
			}
			continue
		}
		if stored.RestSeenAt.After(bar.RestSeenAt) {
			bar.RestSeenAt = stored.RestSeenAt
		}
		bar.Seq = s.nextSeq()
		s.Bars[idx] = bar
		out.Corrected++
		out.Changes = append(out.Changes, feedBarChange{OpenMs: bar.OpenMs, Fields: feedBarChangedFields(stored, bar), PriorSource: stored.Source})
	}
	s.trim()
	return out
}

func correctionDueFrom(s *feedKeyState, now time.Time, offsets []time.Duration) (int64, bool) {
	if len(offsets) == 0 {
		return 0, false
	}
	last := offsets[len(offsets)-1]
	var earliestDue, nextUpcoming time.Time
	var from int64
	for i := len(s.Bars) - 1; i >= 0; i-- {
		b := s.Bars[i]
		closeAt := feedBarCloseAt(b, s.IntervalMs)
		if closeAt.After(now) {
			continue
		}
		if now.After(closeAt.Add(last + feedCorrectionGiveUp)) {
			break
		}
		for _, off := range offsets {
			cp := closeAt.Add(off)
			if !cp.After(b.RestSeenAt) {
				continue
			}
			if !cp.After(now) {
				if earliestDue.IsZero() || cp.Before(earliestDue) {
					earliestDue = cp
				}
				from = b.OpenMs
			} else if nextUpcoming.IsZero() || cp.Before(nextUpcoming) {
				nextUpcoming = cp
			}
			break
		}
	}
	if earliestDue.IsZero() {
		return 0, false
	}
	if !nextUpcoming.IsZero() && nextUpcoming.Sub(earliestDue) <= feedCorrectionCoalesce {
		return 0, false
	}
	return from, true
}

func correctionUnconfirmed(s *feedKeyState, requestedAt time.Time, fromOpenMs int64, offsets []time.Duration) (int, int64) {
	if len(offsets) == 0 {
		return 0, 0
	}
	last := offsets[len(offsets)-1]
	count := 0
	var first int64
	for _, b := range s.Bars {
		if b.OpenMs < fromOpenMs {
			continue
		}
		closeAt := feedBarCloseAt(b, s.IntervalMs)
		if closeAt.After(requestedAt) {
			break
		}
		if requestedAt.After(closeAt.Add(last + feedCorrectionGiveUp)) {
			continue
		}
		for _, off := range offsets {
			cp := closeAt.Add(off)
			if !cp.After(b.RestSeenAt) {
				continue
			}
			if !cp.After(requestedAt) {
				if count == 0 {
					first = b.OpenMs
				}
				count++
			}
			break
		}
	}
	return count, first
}

func (o *marketFeedOwner) finalizeCorrectionLocked(st *feedKeyState, now time.Time) {
	offsets := o.corrOffsets
	if len(offsets) == 0 {
		return
	}
	last := offsets[len(offsets)-1]
	silent := !st.corr.started
	st.corr.started = true
	pending, overdue := 0, 0
	for _, b := range st.Bars {
		closeAt := feedBarCloseAt(b, st.IntervalMs)
		if closeAt.After(now) {
			break
		}
		final := closeAt.Add(last)
		verified := !final.After(b.RestSeenAt)
		if now.After(final.Add(feedCorrectionGiveUp)) {
			if b.OpenMs > st.corr.finalized {
				if !verified && !silent {
					o.correction.Unverified++
					detail := st.corr.lastErr
					if detail == "" {
						detail = "no read confirmed the final checkpoint"
					}
					o.logf("[feed-correction] key=%s open_ms=%d status=unverified final_checkpoint=close+%s gave_up_after=%s reason=%q",
						st.Key, b.OpenMs, last, feedCorrectionGiveUp, detail)
				}
				st.corr.finalized = b.OpenMs
			}
			continue
		}
		if verified {
			continue
		}
		pending++
		if now.After(final.Add(feedCorrectionOverdue)) {
			overdue++
		}
	}
	st.corr.pendingBars, st.corr.overdueBars = pending, overdue
	switch {
	case overdue > 0 && !st.corr.overdue:
		st.corr.overdue = true
		detail := fmt.Sprintf("%d closed bar(s) not re-read by close+%s plus %s; last error: %s",
			overdue, last, feedCorrectionOverdue, firstNonEmpty(st.corr.lastErr, "none"))
		o.logf("[feed-correction] key=%s status=overdue detail=%q", st.Key, detail)
		o.raiseAlert(st.Key, "correction_overdue", detail)
	case overdue == 0 && st.corr.overdue:
		st.corr.overdue = false
		o.logf("[feed-correction] key=%s status=recovered: no closed bar is overdue", st.Key)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (o *marketFeedOwner) correctionPass() []feedCorrectionJob {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	now := o.now()
	keys := make([]marketFeedKey, 0, len(o.keys))
	for k := range o.keys {
		keys = append(keys, k)
	}
	sortMarketFeedKeys(keys)
	var jobs []feedCorrectionJob
	for _, key := range keys {
		st := o.keys[key]
		o.finalizeCorrectionLocked(st, now)
		if st.Status != feedStatusReady || st.corr.inflight || now.Before(st.corr.retryAt) {
			continue
		}
		from, ok := correctionDueFrom(st, now, o.corrOffsets)
		if !ok {
			continue
		}
		st.corr.inflight = true
		jobs = append(jobs, feedCorrectionJob{key: key, st: st, fromOpenMs: from, retry: st.corr.failures > 0})
	}
	return jobs
}

func (o *marketFeedOwner) releaseCorrection(job feedCorrectionJob) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.keys[job.key] == job.st {
		job.st.corr.inflight = false
	}
}

func (o *marketFeedOwner) correctionFailedLocked(st *feedKeyState, err error, refused bool, attempt feedRestReason) {
	st.corr.failures++
	st.corr.lastErr = err.Error()
	backoff := feedCorrectionRetryMin << (st.corr.failures - 1)
	if backoff > feedCorrectionRetryMax || backoff <= 0 {
		backoff = feedCorrectionRetryMax
	}
	st.corr.retryAt = o.now().Add(backoff)
	if refused {
		o.correction.Refused++
	} else {
		o.correction.Failed++
	}
	o.logf("[feed-correction] key=%s status=read_failed reason=%s attempt=%d retry_in=%s error=%q",
		st.Key, attempt, st.corr.failures, backoff, err.Error())
}

func (o *marketFeedOwner) runCorrectionJob(ctx context.Context, job feedCorrectionJob) {
	reason := feedRestCorrection
	if job.retry {
		reason = feedRestCorrectionRetry
	}
	rctx, cancel := context.WithTimeout(withFeedReason(ctx, string(reason)), feedCorrectionReadTimeout)
	requestedAt := o.now()
	raws, err := fetchHyperliquidCandleSnapshotFn(rctx, job.key.Symbol, job.key.Timeframe, job.fromOpenMs, requestedAt.UnixMilli())
	cancel()
	if ctx.Err() != nil {
		o.releaseCorrection(job)
		return
	}
	refused := errors.Is(err, errFeedBudgetExhausted)
	if !refused {
		o.countRestCall(reason)
	}

	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[job.key]
	if st != job.st {
		return
	}
	st.corr.inflight = false
	if !refused {
		o.correction.Reads++
		if job.retry {
			o.correction.Retries++
		}
	}
	if err != nil {
		o.correctionFailedLocked(st, err, refused, reason)
		return
	}
	if len(raws) == 0 {
		o.correctionFailedLocked(st, errors.New("the venue returned no candles for the correction window"), false, reason)
		return
	}
	out := mergeCorrectionRows(st, raws, requestedAt)
	now := o.now()
	o.correction.Corrected += out.Corrected
	o.correction.Added += out.Added
	o.correction.RestOlder += out.RestOlder
	for _, ch := range out.Changes {
		closeAt := time.UnixMilli(ch.OpenMs + st.IntervalMs).UTC()
		prior := string(ch.PriorSource)
		if ch.Added {
			prior = "none"
		}
		line := fmt.Sprintf("key=%s open_ms=%d delay_ms=%d changed=%s prior_source=%s reason=%s",
			st.Key, ch.OpenMs, now.Sub(closeAt).Milliseconds(), ch.Fields, prior, reason)
		o.correction.LastCorrection = now.Format(time.RFC3339) + " " + line
		o.logf("[feed-correction] status=corrected %s", line)
	}
	if n, first := correctionUnconfirmed(st, requestedAt, job.fromOpenMs, o.corrOffsets); n > 0 {
		detail := fmt.Sprintf("the response confirmed no newer value for %d due bar(s) (first open %d; %d invalid, %d older than the stored bar)",
			n, first, out.Invalid, out.RestOlder)
		o.correctionFailedLocked(st, errors.New(detail), false, reason)
		return
	}
	st.corr.failures = 0
	st.corr.lastErr = ""
	st.corr.retryAt = time.Time{}
}

func (o *marketFeedOwner) runCorrection(ctx context.Context) {
	if len(o.corrOffsets) == 0 {
		return
	}
	ticker := time.NewTicker(feedCorrectionTick)
	defer ticker.Stop()
	sem := make(chan struct{}, feedCorrectionParallel)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, job := range o.correctionPass() {
			job := job
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					o.runCorrectionJob(ctx, job)
				}()
			default:
				o.releaseCorrection(job)
			}
		}
	}
}

func (o *marketFeedOwner) correctionHealthLocked() *feedCorrectionHealth {
	if len(o.corrOffsets) == 0 {
		return nil
	}
	h := &feedCorrectionHealth{feedCorrectionStats: o.correction}
	for _, off := range o.corrOffsets {
		h.Offsets = append(h.Offsets, off.String())
	}
	for _, st := range o.keys {
		h.Pending += st.corr.pendingBars
		h.Overdue += st.corr.overdueBars
	}
	return h
}

func formatFeedCorrectionOffsets(offsets []time.Duration) string {
	parts := make([]string, 0, len(offsets))
	for _, off := range offsets {
		parts = append(parts, "+"+off.String())
	}
	return strings.Join(parts, ",")
}
