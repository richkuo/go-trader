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
	WeightRefused      []string
	Paused             []string
	Failed             []string
	RefreshedKeys      map[marketFeedKey]bool
	ReadyOnlyRefreshed bool
	DueKeys            map[marketFeedKey]bool
	InflightKeys       map[marketFeedKey]bool
	DeferredKeys       map[marketFeedKey]time.Time
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
	if err == nil || errors.Is(err, errFeedBudgetExhausted) || feedRateLimitErr(err) || feedFetchTypedResult(err) || ctx.Err() != nil {
		return false
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.backoff+r.margin {
		return false
	}
	return true
}

func (r *feedRESTSource) prepare(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
	rep := feedPrepareReport{RefreshedKeys: make(map[marketFeedKey]bool), ReadyOnlyRefreshed: true,
		DueKeys: make(map[marketFeedKey]bool), InflightKeys: make(map[marketFeedKey]bool), DeferredKeys: make(map[marketFeedKey]time.Time)}
	midsWeight, _ := hlInfoWeight("allMids", 0)
	keyed := withFeedKeepWeight(withFeedKeep(ctx, 1), midsWeight)
	for _, cr := range restRefreshOrder(reqs, coverage) {
		key := cr.Key
		if _, tracked := r.owner.readinessFor(key); !tracked || !r.owner.publishedKey(key) {
			continue
		}
		if !feedKeyDueAt(coverage.KeyCadences[key], deadline) {
			continue
		}
		rep.DueKeys[key] = true
		t, err := r.owner.admitKeyFetch(key, feedRestRefresh, false)
		if err == nil {
			err = r.owner.executeKeyFetch(keyed, t)
			if r.canRetry(ctx, err) && sleepCtx(ctx, r.backoff) == nil {
				if rt, rerr := r.owner.admitKeyFetch(key, feedRestRetry, false); rerr == nil {
					t, err = rt, r.owner.executeKeyFetch(keyed, rt)
				}
			}
		}
		if err == nil {
			rep.Refreshed++
			rep.RefreshedKeys[key] = true
			continue
		}
		var notDue *feedHistoryNotDueError
		switch {
		case errors.As(err, &notDue):
			rep.DeferredKeys[key] = notDue.DueAt
		case errors.Is(err, errFeedKeyInflight):
			rep.InflightKeys[key] = true
		case feedFetchTypedResult(err):
		case errors.Is(err, errFeedWeightRefused):
			r.owner.markTicketUnready(t, feedStatusBudget, fmt.Sprintf("refresh for deadline %d was refused by the weight allocation: %v", deadline, err))
			r.owner.markTicketSharedHeld(t, feedSharedHeldErr(err))
			rep.WeightRefused = append(rep.WeightRefused, key.PayloadID())
		case errors.Is(err, errFeedBudgetExhausted):
			r.owner.markTicketUnready(t, feedStatusBudget, fmt.Sprintf("refresh for deadline %d was refused: %v", deadline, err))
			rep.Refused = append(rep.Refused, key.PayloadID())
		case feedRateLimitErr(err):
			r.owner.markTicketRateLimited(t, fmt.Sprintf("refresh for deadline %d was held by a venue rate limit: %v", deadline, err))
			rep.Paused = append(rep.Paused, key.PayloadID())
		default:
			r.owner.markTicketUnready(t, feedStatusFailed, fmt.Sprintf("refresh for deadline %d failed: %v", deadline, err))
			rep.Failed = append(rep.Failed, key.PayloadID())
		}
		r.logf("[feed] %s: refresh for deadline %d: %v", key, deadline, err)
	}

	r.owner.EnsureFunding(keyed, r.owner.earliestFrameBarMs(reqs))
	r.owner.EnsureAccountingFunding(keyed)

	if len(reqs.Coins) > 0 {
		midCtx := withFeedInput(withFeedReason(ctx, string(feedRestMids)), "mids")
		mids, err := fetchHyperliquidMidsCtxFn(midCtx, reqs.Coins)
		if r.canRetry(ctx, err) && sleepCtx(ctx, r.backoff) == nil {
			mids, err = fetchHyperliquidMidsCtxFn(withFeedInput(withFeedReason(ctx, string(feedRestRetry)), "mids"), reqs.Coins)
		}
		switch {
		case err == nil:
			r.owner.IngestMids(mids, r.owner.now(), feedSourceREST)
			r.owner.setMidsRateLimited(false)
			r.owner.setMidsWeightHeld(false)
		case errors.Is(err, errFeedWeightRefused):
			r.owner.setMidsWeightHeld(true)
			rep.WeightRefused = append(rep.WeightRefused, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		case errors.Is(err, errFeedBudgetExhausted):
			rep.Refused = append(rep.Refused, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		case feedRateLimitErr(err):
			r.owner.setMidsRateLimited(true)
			rep.Paused = append(rep.Paused, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		default:
			rep.Failed = append(rep.Failed, "mids")
			r.logf("[feed] mids for deadline %d: %v", deadline, err)
		}
	}
	return rep
}
