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

	feedRESTCandleWorkers = 4
	feedRESTMidsSlot      = 6 * time.Second
	feedRESTMidsHold      = 8 * time.Second
	feedRESTFreezeReserve = 500 * time.Millisecond
	feedIncrementalRows   = 3
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
	ReusedKeys         map[marketFeedKey]bool
	ReuseRefused       map[marketFeedKey]string
	ReuseProofFetched  int
	ReusePaused        bool
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

const feedReuseDayMs = int64(86_400_000)

func feedReuseAlignedTimeframe(timeframe string) bool {
	ms, ok := hlCandleIntervalMs(timeframe)
	return ok && ms > 0 && ms <= feedReuseDayMs && feedReuseDayMs%ms == 0
}

func feedClosedFrameReuse(bars []feedBar, r feedKeyReadiness, restVerified bool, intervalMs, cutoffMs int64, now time.Time) ([]feedBar, string) {
	if !restVerified {
		return nil, "no successful REST refresh of this key since its last failure, refusal, pause, history load or lookback change"
	}
	if !r.Ready || r.Stale {
		return nil, fmt.Sprintf("the key is not ready (status %s)", r.Status)
	}
	if intervalMs <= 0 || feedReuseDayMs%intervalMs != 0 {
		return nil, "the candle interval is not aligned to UTC days"
	}
	boundary := cutoffMs / intervalMs * intervalMs
	latestOpen := boundary - intervalMs
	closed := 0
	for closed < len(bars) && bars[closed].CloseMs < cutoffMs {
		closed++
	}
	if closed == 0 {
		return nil, "no stored bar closed before the seal cutoff"
	}
	last := bars[closed-1]
	if last.OpenMs != latestOpen || last.CloseMs != boundary-1 {
		return nil, fmt.Sprintf("the newest stored closed bar opens at %d, not at %d, the latest bar closed at the seal cutoff", last.OpenMs, latestOpen)
	}
	for i := 0; i < closed; i++ {
		if bars[i].RestSeenAt.IsZero() || bars[i].RestSeenAt.UnixMilli() <= bars[i].CloseMs {
			return nil, fmt.Sprintf("the bar opened at %d was not read from REST after it closed", bars[i].OpenMs)
		}
	}
	if closed < r.Required || closed < feedReadyFloor(r.Required) {
		return nil, fmt.Sprintf("%d closed bars are stored, below the %d-bar requirement", closed, r.Required)
	}
	if now.UTC().UnixMilli()-last.CloseMs > 2*intervalMs+int64(feedStaleGrace/time.Millisecond) {
		return nil, "the newest closed bar is stale"
	}
	return append([]feedBar(nil), bars[:closed]...), ""
}

func (o *marketFeedOwner) closedFrameReuseProof(key marketFeedKey, cutoffMs int64) string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil || !o.published[key] {
		return "the key is not published"
	}
	if st.historyInflight {
		return "a history request for this key is in flight"
	}
	now := o.now()
	_, why := feedClosedFrameReuse(st.Bars, keyReadiness(st, now, o.connectedForReadinessLocked(now)), st.restVerified, st.IntervalMs, cutoffMs, now)
	return why
}

func (o *marketFeedOwner) dropReuseProof(keys map[marketFeedKey]string) {
	if len(keys) == 0 {
		return
	}
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	for key := range keys {
		if st := o.keys[key]; st != nil {
			st.restVerified = false
		}
	}
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

type feedRESTRead struct {
	key    marketFeedKey
	ticket *feedFetchTicket
	err    error
	sent   bool
}

func (r *feedRESTSource) readKeys(ctx context.Context, reads []*feedRESTRead, reason feedRestReason) {
	pool := newFeedOrderedPool(feedRESTCandleWorkers)
	for _, rd := range reads {
		pool.acquire()
		if err := ctx.Err(); err != nil {
			rd.err = fmt.Errorf("the candle window of this preparation ended before the read was admitted: %w", err)
			pool.release()
			continue
		}
		t, err := r.owner.admitKeyFetch(rd.key, reason, false)
		if err != nil {
			rd.err = err
			pool.release()
			continue
		}
		rd.ticket, rd.sent = t, true
		pool.start(ctx, func(jctx context.Context) { rd.err = r.owner.executeKeyFetch(jctx, t) })
	}
	pool.wg.Wait()
}

func feedPhaseContext(ctx context.Context, reserve time.Duration) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	if left := time.Until(dl); left <= 2*reserve {
		reserve = left / 2
	}
	return context.WithDeadline(ctx, dl.Add(-reserve))
}

func feedUpcomingDeadlines(cadences map[marketFeedKey][]int, deadline int64, span time.Duration) []int64 {
	set := make(map[int]bool)
	for _, list := range cadences {
		for _, c := range list {
			if c > 0 {
				set[c] = true
			}
		}
	}
	seen := make(map[int64]bool)
	var out []int64
	for c := range set {
		step := int64(c)
		for d := (deadline/step + 1) * step; d-deadline <= int64(span/time.Second); d += step {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func feedFundingPlanFor(deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedFundingPlan {
	plan := feedFundingPlan{Needs: reqs.Funding, CoverageID: reqs.FundingCoverage, CutoffMs: deadline * 1000}
	upcoming := feedUpcomingDeadlines(coverage.KeyCadences, deadline, feedAccountingSlack)
	for _, d := range upcoming {
		plan.Upcoming = append(plan.Upcoming, time.Unix(d, 0).UTC().Add(feedSealSettleDelay))
	}
	if len(upcoming) == 0 {
		return plan
	}
	candleW, _ := hlInfoWeight("candleSnapshot", feedIncrementalRows)
	midsW, _ := hlInfoWeight("allMids", 0)
	for _, cr := range reqs.Keys {
		if feedKeyDueAt(coverage.KeyCadences[cr.Key], upcoming[0]) {
			plan.NextWeight += candleW
			plan.NextReads++
		}
	}
	if len(reqs.Coins) > 0 {
		plan.NextWeight += midsW
		plan.NextReads++
	}
	return plan
}

func (r *feedRESTSource) prepare(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
	rep := feedPrepareReport{RefreshedKeys: make(map[marketFeedKey]bool), ReadyOnlyRefreshed: true,
		DueKeys: make(map[marketFeedKey]bool), InflightKeys: make(map[marketFeedKey]bool), DeferredKeys: make(map[marketFeedKey]time.Time),
		ReusedKeys: make(map[marketFeedKey]bool), ReuseRefused: make(map[marketFeedKey]string)}
	r.prepareCandles(ctx, deadline, reqs, coverage, &rep)
	midsAt, read := r.prepareMids(ctx, deadline, reqs, &rep)
	fundingCtx, cancel := feedPhaseContext(ctx, feedRESTFreezeReserve)
	defer cancel()
	if read {
		var cancelMids context.CancelFunc
		fundingCtx, cancelMids = context.WithDeadline(fundingCtx, midsAt.Add(feedRESTMidsHold))
		defer cancelMids()
	}
	r.owner.EnsureFunding(fundingCtx, feedFundingPlanFor(deadline, reqs, coverage))
	return rep
}

func (r *feedRESTSource) prepareCandles(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements, rep *feedPrepareReport) {
	cutoffMs := deadline * 1000
	midsWeight, _ := hlInfoWeight("allMids", 0)
	candleCtx, cancel := feedPhaseContext(ctx, feedRESTMidsSlot+feedRESTFreezeReserve)
	defer cancel()
	keyed := withFeedKeepWeight(withFeedKeep(candleCtx, 1), midsWeight)
	var reads []*feedRESTRead
	for _, cr := range restRefreshOrder(reqs, coverage) {
		key := cr.Key
		if _, tracked := r.owner.readinessFor(key); !tracked || !r.owner.publishedKey(key) {
			continue
		}
		if !feedKeyDueAt(coverage.KeyCadences[key], deadline) {
			continue
		}
		rep.DueKeys[key] = true
		if coverage.closedOnlyReuse(key) {
			if r.owner.closedFrameReuseProof(key, cutoffMs) == "" {
				rep.ReusedKeys[key] = true
				continue
			}
			rep.ReuseProofFetched++
		}
		reads = append(reads, &feedRESTRead{key: key})
	}
	r.readKeys(keyed, reads, feedRestRefresh)
	var retries []*feedRESTRead
	for _, rd := range reads {
		if rd.sent && r.canRetry(candleCtx, rd.err) {
			retries = append(retries, rd)
		}
	}
	if len(retries) > 0 && sleepCtx(candleCtx, r.backoff) == nil {
		again := make([]*feedRESTRead, 0, len(retries))
		for _, rd := range retries {
			again = append(again, &feedRESTRead{key: rd.key})
		}
		r.readKeys(keyed, again, feedRestRetry)
		for i, rd := range again {
			if rd.sent {
				retries[i].ticket, retries[i].err = rd.ticket, rd.err
			}
		}
	}
	for _, rd := range reads {
		r.recordCandleRead(deadline, rd, rep)
	}
}

func (r *feedRESTSource) recordCandleRead(deadline int64, rd *feedRESTRead, rep *feedPrepareReport) {
	key, t, err := rd.key, rd.ticket, rd.err
	if err == nil {
		rep.Refreshed++
		rep.RefreshedKeys[key] = true
		return
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

func (r *feedRESTSource) prepareMids(ctx context.Context, deadline int64, reqs cycleMarketRequirements, rep *feedPrepareReport) (time.Time, bool) {
	if len(reqs.Coins) == 0 {
		return time.Time{}, false
	}
	midsCtx, cancel := context.WithTimeout(ctx, feedRESTMidsSlot)
	defer cancel()
	midsCtx = withFeedInput(withFeedReason(midsCtx, string(feedRestMids)), "mids")
	mids, err := fetchHyperliquidMidsCtxFn(midsCtx, reqs.Coins)
	if r.canRetry(midsCtx, err) && sleepCtx(midsCtx, r.backoff) == nil {
		mids, err = fetchHyperliquidMidsCtxFn(withFeedReason(midsCtx, string(feedRestRetry)), reqs.Coins)
	}
	if err == nil {
		r.owner.IngestMids(mids, r.owner.now(), feedSourceREST)
		r.owner.setMidsRateLimited(false)
		if r.owner.validateMidsWeight(mids) {
			feedLedgerFrom(ctx).weightInputValidated("mids")
		}
		return time.Now(), true
	}
	switch {
	case errors.Is(err, errFeedWeightRefused):
		r.owner.holdMidsWeight(reqs.Coins)
		rep.WeightRefused = append(rep.WeightRefused, "mids")
	case errors.Is(err, errFeedBudgetExhausted):
		rep.Refused = append(rep.Refused, "mids")
	case feedRateLimitErr(err):
		r.owner.setMidsRateLimited(true)
		rep.Paused = append(rep.Paused, "mids")
	default:
		rep.Failed = append(rep.Failed, "mids")
	}
	r.logf("[feed] mids for deadline %d: %v", deadline, err)
	return time.Time{}, false
}

func formatFeedReuseLines(deadline int64, source, instance string, rep feedPrepareReport) []string {
	reused := make([]string, 0, len(rep.ReusedKeys))
	for k := range rep.ReusedKeys {
		reused = append(reused, k.PayloadID())
	}
	sort.Strings(reused)
	refusedKeys := make([]marketFeedKey, 0, len(rep.ReuseRefused))
	for k := range rep.ReuseRefused {
		refusedKeys = append(refusedKeys, k)
	}
	sortMarketFeedKeys(refusedKeys)
	refused := make([]string, 0, len(refusedKeys))
	for _, k := range refusedKeys {
		refused = append(refused, fmt.Sprintf("%s(%s)", k.PayloadID(), rep.ReuseRefused[k]))
	}
	return []string{fmt.Sprintf("[feed-reuse] key=%d source=%s instance=%s reused=%d reused_keys=%s refused=%d refused_keys=%s",
		deadline, source, instance, len(reused), joinFeedNames(reused), len(refused), joinFeedNames(refused))}
}
