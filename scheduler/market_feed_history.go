package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

const (
	feedHistoryRetryBase     = 5 * time.Second
	feedHistoryRetryCap      = 5 * time.Minute
	feedHistoryRefusalFloor  = 60 * time.Second
	feedHistoryWorkers       = 4
	feedHistoryTick          = time.Second
	feedHistoryEmptyResponse = "the venue returned no history"
	feedHistoryAlertKind     = "history_failed"
)

var (
	errFeedKeyInflight    = errors.New("a request for this feed key is already in flight")
	errFeedHistoryPending = errors.New("full history for this feed key is not loaded; the history recovery owns that load")
	errFeedHistoryNotDue  = errors.New("the full-history retry for this feed key is not due yet")
	errFeedKeyObsolete    = errors.New("the feed key was removed or replaced while its request was in flight")
)

type feedHistoryNotDueError struct {
	Key   marketFeedKey
	DueAt time.Time
}

func (e *feedHistoryNotDueError) Error() string {
	return fmt.Sprintf("%v: %s is due at %s", errFeedHistoryNotDue, e.Key, e.DueAt.UTC().Format(time.RFC3339))
}

func (e *feedHistoryNotDueError) Unwrap() error { return errFeedHistoryNotDue }

func feedFetchTypedResult(err error) bool {
	return errors.Is(err, errFeedKeyInflight) || errors.Is(err, errFeedHistoryPending) ||
		errors.Is(err, errFeedHistoryNotDue) || errors.Is(err, errFeedKeyObsolete)
}

func feedHistoryRefusal(err error) bool {
	return feedRateLimitErr(err) || errors.Is(err, errFeedBudgetExhausted) || errors.Is(err, errFeedWeightRefused)
}

func feedHistoryRetryFloor(failures int, refusal bool) time.Duration {
	base := feedHistoryRetryBase
	for i := 1; i < failures && base < feedHistoryRetryCap; i++ {
		base *= 2
	}
	if base > feedHistoryRetryCap {
		base = feedHistoryRetryCap
	}
	if refusal && base < feedHistoryRefusalFloor {
		base = feedHistoryRefusalFloor
	}
	return base
}

func feedHistoryJitterSpan(floor time.Duration) time.Duration {
	span := floor / 2
	if room := feedHistoryRetryCap - floor; room < span {
		span = room
	}
	if span < 0 {
		span = 0
	}
	return span
}

func feedHistoryRandomJitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(span) + 1))
}

type feedFetchTicket struct {
	st         *feedKeyState
	key        marketFeedKey
	reason     feedRestReason
	full       bool
	lookback   int
	startMs    int64
	intervalMs int64
	seq        uint64
	loop       bool
}

func (o *marketFeedOwner) admitKeyFetch(key marketFeedKey, reason feedRestReason, incrementalOnly bool) (*feedFetchTicket, error) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.admitKeyFetchLocked(key, reason, incrementalOnly, o.now())
}

func (o *marketFeedOwner) admitKeyFetchLocked(key marketFeedKey, reason feedRestReason, incrementalOnly bool, now time.Time) (*feedFetchTicket, error) {
	st := o.keys[key]
	if st == nil {
		return nil, fmt.Errorf("feed key %s is not tracked", key)
	}
	if st.historyInflight {
		return nil, fmt.Errorf("feed key %s: %w", key, errFeedKeyInflight)
	}
	full := st.needsHistory()
	if full && incrementalOnly {
		return nil, fmt.Errorf("feed key %s (%d of %d bars of lookback loaded): %w", key, st.historyLookback, st.historyTarget, errFeedHistoryPending)
	}
	if full && now.Before(st.historyRetryAt) {
		return nil, &feedHistoryNotDueError{Key: key, DueAt: st.historyRetryAt}
	}
	st.historyInflight = true
	o.fetchSeq++
	t := &feedFetchTicket{st: st, key: key, reason: reason, full: full, lookback: st.historyTarget, intervalMs: st.IntervalMs, seq: o.fetchSeq}
	if !full {
		t.startMs = st.Bars[len(st.Bars)-1].OpenMs
	}
	return t, nil
}

func (o *marketFeedOwner) fetchAndMerge(ctx context.Context, key marketFeedKey, reason feedRestReason) error {
	t, err := o.admitKeyFetch(key, reason, false)
	if err != nil {
		return err
	}
	return o.executeKeyFetch(ctx, t)
}

func (o *marketFeedOwner) fetchIncremental(ctx context.Context, key marketFeedKey, reason feedRestReason) error {
	t, err := o.admitKeyFetch(key, reason, true)
	if err != nil {
		return err
	}
	return o.executeKeyFetch(ctx, t)
}

func (o *marketFeedOwner) executeKeyFetch(ctx context.Context, t *feedFetchTicket) (err error) {
	released := false
	defer func() {
		if !released {
			o.feedMu.Lock()
			t.st.historyInflight = false
			o.feedMu.Unlock()
		}
	}()
	ctx = withFeedInput(withFeedReason(ctx, string(t.reason)), t.key.PayloadID())
	requestedAt := o.now()
	var hist hlCandleHistory
	if t.full {
		hist, err = hlFetchCandleHistory(ctx, t.key.Symbol, t.key.Timeframe, t.lookback, requestedAt)
	} else {
		windowStart := t.startMs - t.intervalMs
		if windowStart < 0 {
			windowStart = 0
		}
		hist.Raws, err = fetchHyperliquidCandleSnapshotFn(ctx, t.key.Symbol, t.key.Timeframe, windowStart, requestedAt.UnixMilli())
	}
	if !errors.Is(err, errFeedHostPaused) {
		o.countRestCall(t.reason)
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	released = true
	t.st.historyInflight = false
	return o.completeKeyFetchLocked(t, hist, err, requestedAt)
}

func (o *marketFeedOwner) completeKeyFetchLocked(t *feedFetchTicket, hist hlCandleHistory, err error, requestedAt time.Time) error {
	key := t.key
	if o.keys[key] != t.st {
		return fmt.Errorf("feed key %s: %w", key, errFeedKeyObsolete)
	}
	st := t.st
	now := o.now()
	if feedRateLimitErr(err) {
		o.markRateLimitedLocked(st, err.Error())
		if t.full {
			o.historyFailedLocked(t, err, now)
		}
		return err
	}
	if err != nil {
		weight := errors.Is(err, errFeedWeightRefused)
		st.StatusDetail = err.Error()
		if weight {
			st.weightHeld = true
			st.sharedHeld = feedSharedHeldErr(err)
		}
		if t.full {
			o.historyFailedLocked(t, err, now)
		}
		if len(st.Bars) == 0 || st.historyLookback == 0 {
			st.Status = feedStatusFailed
			if weight {
				st.Status = feedStatusBudget
			}
		}
		return err
	}

	out := mergeRestRows(st, hist.Raws, requestedAt)
	drainPendingSocketBars(st)
	if st.rlSeq >= t.seq {
		return fmt.Errorf("feed key %s: this fetch started before the latest rate-limit mark, so the key stays %s until a later fetch succeeds: %w",
			key, feedStatusRateLimited, errFeedHostPaused)
	}
	st.rlSeq = 0
	st.weightHeld = false
	st.sharedHeld = false
	if t.full {
		if verr := feedHistoryCompletionError(t, hist, out); verr != nil {
			o.historyFailedLocked(t, verr, now)
			if st.Status != feedStatusReady && st.Status != feedStatusRepairing {
				st.Status = feedStatusFailed
				st.StatusDetail = verr.Error()
			}
			return verr
		}
		attempts := st.historyFailures + 1
		recovered := t.loop || st.historyFailures > 0
		st.historyLookback = t.lookback
		st.historyShort = out.Stored < t.lookback
		st.historyErr = ""
		st.historyFailures = 0
		st.historyRetryAt = time.Time{}
		if recovered && t.lookback >= st.historyTarget {
			o.logf("[feed-history] key=%s status=recovered bars=%d required=%d attempts=%d", key, len(st.Bars), t.lookback, attempts)
		}
	}
	if len(st.Bars) == 0 {
		st.Status = feedStatusFailed
		st.StatusDetail = "the venue returned no candles"
		return fmt.Errorf("feed key %s: the venue returned no candles", key)
	}
	st.Status = feedStatusReady
	st.StatusDetail = ""
	return nil
}

func feedHistoryCompletionError(t *feedFetchTicket, hist hlCandleHistory, out mergeRestOutcome) error {
	if len(hist.Raws) == 0 {
		return fmt.Errorf("feed key %s: %s", t.key, feedHistoryEmptyResponse)
	}
	if out.Invalid > 0 {
		return fmt.Errorf("feed key %s: %d of %d history rows were invalid, so the %d-bar load is not complete",
			t.key, out.Invalid, len(hist.Raws), t.lookback)
	}
	if out.Stored >= t.lookback || hist.Exhausted {
		return nil
	}
	return fmt.Errorf("feed key %s: the history load stored %d distinct bars of the %d-bar lookback and did not reach the venue history boundary",
		t.key, out.Stored, t.lookback)
}

func (o *marketFeedOwner) historyFailedLocked(t *feedFetchTicket, err error, now time.Time) {
	st := t.st
	if st.needsHistory() {
		st.historyErr = err.Error()
	}
	st.historyFailures++
	floor := feedHistoryRetryFloor(st.historyFailures, feedHistoryRefusal(err))
	jitter := o.historyJitter
	if jitter == nil {
		jitter = feedHistoryRandomJitter
	}
	extra := jitter(feedHistoryJitterSpan(floor))
	if span := feedHistoryJitterSpan(floor); extra > span {
		extra = span
	}
	if extra < 0 {
		extra = 0
	}
	due := now.Add(floor + extra)
	var paused *feedHostPausedError
	if errors.As(err, &paused) && paused.Until.After(due) {
		due = paused.Until
	}
	st.historyRetryAt = due
	o.logf("[feed-history] key=%s status=failed reason=%s attempt=%d retry_in=%s error=%v",
		t.key, t.reason, st.historyFailures, due.Sub(now).Round(time.Millisecond), err)
}

func (o *marketFeedOwner) historyPass(slots int) []*feedFetchTicket {
	if slots <= 0 {
		return nil
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	now := o.now()
	type candidate struct {
		key marketFeedKey
		due time.Time
	}
	var due []candidate
	for key, st := range o.keys {
		if st.historyInflight || !st.needsHistory() || now.Before(st.historyRetryAt) {
			continue
		}
		due = append(due, candidate{key: key, due: st.historyRetryAt})
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].due.Equal(due[j].due) {
			return due[i].due.Before(due[j].due)
		}
		return marketFeedKeyLess(due[i].key, due[j].key)
	})
	var out []*feedFetchTicket
	for _, c := range due {
		if len(out) >= slots {
			break
		}
		t, err := o.admitKeyFetchLocked(c.key, feedRestRecovery, false, now)
		if err != nil {
			continue
		}
		t.loop = true
		out = append(out, t)
	}
	return out
}

func (o *marketFeedOwner) runHistoryJob(ctx context.Context, t *feedFetchTicket) error {
	jobCtx, cancel := context.WithTimeout(ctx, feedBootstrapKeyBudget)
	err := o.executeKeyFetch(jobCtx, t)
	cancel()
	if err == nil || ctx.Err() != nil || feedFetchTypedResult(err) || feedHistoryRefusal(err) {
		return err
	}
	o.raiseAlert(t.key, feedHistoryAlertKind, err.Error())
	return err
}

func (o *marketFeedOwner) runHistoryRecovery(ctx context.Context) {
	o.feedMu.Lock()
	tick := o.historyTick
	workers := o.historyWorkers
	o.feedMu.Unlock()
	if tick <= 0 {
		tick = feedHistoryTick
	}
	if workers < 2 {
		workers = feedHistoryWorkers
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, t := range o.historyPass(cap(sem) - len(sem)) {
			t := t
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				o.runHistoryJob(ctx, t)
			}()
		}
	}
}

func (o *marketFeedOwner) markTicketUnready(t *feedFetchTicket, status feedKeyStatus, detail string) {
	if t == nil {
		return
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.keys[t.key] != t.st {
		return
	}
	t.st.Status = status
	t.st.StatusDetail = detail
}

func (o *marketFeedOwner) markTicketSharedHeld(t *feedFetchTicket, held bool) {
	if t == nil {
		return
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.keys[t.key] != t.st {
		return
	}
	t.st.sharedHeld = held
}

func (o *marketFeedOwner) markTicketRateLimited(t *feedFetchTicket, detail string) {
	if t == nil {
		return
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if o.keys[t.key] != t.st {
		return
	}
	o.markRateLimitedLocked(t.st, detail)
}
