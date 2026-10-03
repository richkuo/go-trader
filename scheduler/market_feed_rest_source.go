package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	feedRESTRetryBackoff = time.Second
	feedRESTRetryMargin  = 3 * time.Second
)

type feedPrepareReport struct {
	Refreshed          int
	Refused            []string
	Failed             []string
	RefreshedKeys      map[marketFeedKey]bool
	ReadyOnlyRefreshed bool
}

type feedPrepareFunc func(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport

func websocketFeedPrepare(owner *marketFeedOwner) feedPrepareFunc {
	return func(ctx context.Context, _ int64, reqs cycleMarketRequirements, _ feedRequirements) feedPrepareReport {
		prepareMarketSnapshot(ctx, owner, reqs)
		return feedPrepareReport{}
	}
}

type feedRESTSource struct {
	owner   *marketFeedOwner
	logf    func(string, ...any)
	backoff time.Duration
	margin  time.Duration
}

func newFeedRESTSource(owner *marketFeedOwner, logf func(string, ...any)) *feedRESTSource {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &feedRESTSource{owner: owner, logf: logf, backoff: feedRESTRetryBackoff, margin: feedRESTRetryMargin}
}

func feedKeyDueAt(cadences []int, deadline int64) bool {
	if len(cadences) == 0 {
		return true
	}
	for _, c := range cadences {
		if c > 0 && deadline%int64(c) == 0 {
			return true
		}
	}
	return false
}

func restRefreshOrder(reqs cycleMarketRequirements, coverage feedRequirements) []cycleMarketRequirement {
	out := append([]cycleMarketRequirement(nil), reqs.Keys...)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := coverage.SignalKeys[out[i].Key], coverage.SignalKeys[out[j].Key]
		if si != sj {
			return si
		}
		return marketFeedKeyLess(out[i].Key, out[j].Key)
	})
	return out
}

func (r *feedRESTSource) canRetry(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, errFeedBudgetExhausted) || ctx.Err() != nil {
		return false
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.backoff+r.margin {
		return false
	}
	return true
}

func (r *feedRESTSource) prepare(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
	rep := feedPrepareReport{RefreshedKeys: make(map[marketFeedKey]bool), ReadyOnlyRefreshed: true}
	keyed := withFeedKeep(ctx, 1)
	for _, cr := range restRefreshOrder(reqs, coverage) {
		key := cr.Key
		if _, tracked := r.owner.readinessFor(key); !tracked || !r.owner.publishedKey(key) {
			continue
		}
		if !feedKeyDueAt(coverage.KeyCadences[key], deadline) {
			continue
		}
		err := r.owner.fetchAndMerge(keyed, key, feedRestRefresh)
		if r.canRetry(ctx, err) && sleepCtx(ctx, r.backoff) == nil {
			err = r.owner.fetchAndMerge(keyed, key, feedRestRetry)
		}
		if err == nil {
			rep.Refreshed++
			rep.RefreshedKeys[key] = true
			continue
		}
		if errors.Is(err, errFeedBudgetExhausted) {
			r.owner.markKeyUnready(key, feedStatusBudget, fmt.Sprintf("refresh for deadline %d was refused: %v", deadline, err))
			rep.Refused = append(rep.Refused, key.PayloadID())
		} else {
			r.owner.markKeyUnready(key, feedStatusFailed, fmt.Sprintf("refresh for deadline %d failed: %v", deadline, err))
			rep.Failed = append(rep.Failed, key.PayloadID())
		}
		r.logf("[feed] %s: refresh for deadline %d: %v", key, deadline, err)
	}

	r.owner.EnsureFunding(keyed, r.owner.earliestFrameBarMs(reqs))

	if len(reqs.Coins) > 0 {
		midCtx := withFeedReason(ctx, string(feedRestMids))
		mids, err := fetchHyperliquidMidsCtxFn(midCtx, reqs.Coins)
		if r.canRetry(ctx, err) && sleepCtx(ctx, r.backoff) == nil {
			mids, err = fetchHyperliquidMidsCtxFn(withFeedReason(ctx, string(feedRestRetry)), reqs.Coins)
		}
		switch {
		case err == nil:
			r.owner.IngestMids(mids, r.owner.now(), feedSourceREST)
		case errors.Is(err, errFeedBudgetExhausted):
			rep.Refused = append(rep.Refused, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		default:
			rep.Failed = append(rep.Failed, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		}
	}
	return rep
}
