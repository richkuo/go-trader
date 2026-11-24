package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	feedFundingTTL             = 5 * time.Minute
	feedFundingRecordsContract = 1
	feedFundingWorkers         = 4
	feedFundingRowMargin       = 1
	feedFundingTailReads       = 2
	feedFundingReadFloor       = time.Second
	feedFundingTrailingGapMs   = 6 * hlFundingRecordStepMs
	feedFundingDeferralLimit   = 10 * time.Minute
	feedFundingContextInput    = "funding_context"
	feedFundingLoading         = "funding history is loading"
	feedFundingCoverageChanged = "the funding requirements changed during this preparation, so its record coverage is not proved"
)

type feedFundingReader struct {
	Key  marketFeedKey
	Rows int
}

type feedFundingNeed struct {
	Scalar  bool
	Records bool
	Readers []feedFundingReader
}

func (n feedFundingNeed) withReader(key marketFeedKey, rows int) feedFundingNeed {
	readers := make([]feedFundingReader, 0, len(n.Readers)+1)
	found := false
	for _, r := range n.Readers {
		if r.Key == key {
			found = true
			r.Rows = max(r.Rows, rows)
		}
		readers = append(readers, r)
	}
	if !found {
		readers = append(readers, feedFundingReader{Key: key, Rows: rows})
	}
	sort.Slice(readers, func(i, j int) bool { return marketFeedKeyLess(readers[i].Key, readers[j].Key) })
	n.Readers = readers
	return n
}

func (n feedFundingNeed) merge(other feedFundingNeed) feedFundingNeed {
	n.Scalar = n.Scalar || other.Scalar
	n.Records = n.Records || other.Records
	for _, r := range other.Readers {
		n = n.withReader(r.Key, r.Rows)
	}
	return n
}

func (n feedFundingNeed) covers(other feedFundingNeed) []string {
	var gaps []string
	have := make(map[marketFeedKey]int, len(n.Readers))
	for _, r := range n.Readers {
		have[r.Key] = r.Rows
	}
	for _, r := range other.Readers {
		if got := have[r.Key]; got < r.Rows {
			gaps = append(gaps, fmt.Sprintf("%s rows %d < %d", r.Key.PayloadID(), got, r.Rows))
		}
	}
	return gaps
}

func (n feedFundingNeed) rangeHours() int64 {
	longest := int64(0)
	for _, r := range n.Readers {
		ms, ok := hlCandleIntervalMs(r.Key.Timeframe)
		if !ok {
			continue
		}
		longest = max(longest, int64(r.Rows+feedFundingRowMargin+1)*ms)
	}
	return longest / hlFundingRecordStepMs
}

func (n feedFundingNeed) readerList() string {
	parts := make([]string, 0, len(n.Readers))
	for _, r := range n.Readers {
		parts = append(parts, fmt.Sprintf("%s:%d", r.Key.PayloadID(), r.Rows))
	}
	return joinFeedNames(parts)
}

func sortedFundingCoins(needs map[string]feedFundingNeed) []string {
	coins := make([]string, 0, len(needs))
	for coin := range needs {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	return coins
}

func fundingRecordsNeeded(needs map[string]feedFundingNeed) bool {
	for _, need := range needs {
		if need.Records {
			return true
		}
	}
	return false
}

func fundingCoverageID(needs map[string]feedFundingNeed) string {
	if !fundingRecordsNeeded(needs) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "v%d\n", feedFundingRecordsContract)
	for _, coin := range sortedFundingCoins(needs) {
		need := needs[coin]
		if !need.Records {
			continue
		}
		fmt.Fprintf(&b, "%s", coin)
		for _, r := range need.Readers {
			fmt.Fprintf(&b, "|%s=%d", r.Key, r.Rows)
		}
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

type feedFundingRun struct {
	StartMs int64
	EndMs   int64
	Records []feedFundingRecord
	Pages   int
	overlap bool
}

func (r feedFundingRun) boundary() *feedFundingRecord {
	if len(r.Records) == 0 {
		return nil
	}
	b := r.Records[len(r.Records)-1]
	return &b
}

func (r feedFundingRun) cursor() int64 {
	if b := r.boundary(); b != nil {
		return b.TimeMs + 1
	}
	return r.StartMs
}

func (r feedFundingRun) readBound() int {
	from := r.cursor()
	if b := r.boundary(); b != nil && r.overlap {
		from = b.TimeMs
	}
	if from > r.EndMs {
		return 1
	}
	rows, err := hlFundingRowBound(from, r.EndMs)
	if err != nil {
		return hlFundingRowCap
	}
	return rows
}

func (r feedFundingRun) readWeight() int {
	w, err := hlInfoWeight("fundingHistory", r.readBound())
	if err != nil {
		w, _ = hlInfoWeightAtCap("fundingHistory")
	}
	return w
}

func (r feedFundingRun) resumed(endMs int64) feedFundingRun {
	r.EndMs = max(r.EndMs, endMs)
	r.overlap = len(r.Records) > 0
	return r
}

func (r feedFundingRun) next(ctx context.Context, coin string) (feedFundingRun, bool, error) {
	if r.Pages >= hlFundingMaxRangePasses {
		return r, false, fmt.Errorf("fundingHistory %s range [%d, %d] needed more than %d pages: %w", coin, r.StartMs, r.EndMs, hlFundingMaxRangePasses, errFundingRangeIncomplete)
	}
	var boundary *feedFundingRecord
	if r.overlap {
		boundary = r.boundary()
	}
	if boundary == nil && r.cursor() > r.EndMs {
		return r, true, nil
	}
	page, complete, err := hlFundingReadPage(ctx, coin, r.cursor(), r.EndMs, boundary)
	if err != nil {
		return r, false, err
	}
	if !complete && len(page) == 0 && boundary == nil {
		return r, false, fmt.Errorf("fundingHistory %s page from %d made no progress: %w", coin, r.cursor(), errFundingRangeIncomplete)
	}
	if complete && len(page) == 0 && boundary == nil {
		proved := r.cursor() - 1
		if gap := r.EndMs - proved; gap > feedFundingTrailingGapMs {
			return r, false, fmt.Errorf("fundingHistory %s returned no record after %d for a range that ends %dms later, over the %dms a late venue publication can explain: %w", coin, proved, gap, feedFundingTrailingGapMs, errFundingRangeIncomplete)
		}
	}
	out := r
	out.Records = append(append(make([]feedFundingRecord, 0, len(r.Records)+len(page)), r.Records...), page...)
	out.Pages++
	out.overlap = false
	return out, complete, nil
}

func feedFundingThrough(records []feedFundingRecord, endMs int64) int64 {
	if n := len(records); n > 0 {
		lastHour := records[n-1].TimeMs / hlFundingRecordStepMs
		if lastHour >= endMs/hlFundingRecordStepMs {
			return max(endMs, (lastHour+1)*hlFundingRecordStepMs-1)
		}
	}
	return endMs
}

func trimFundingRecords(records []feedFundingRecord, fromMs int64) []feedFundingRecord {
	i := sort.Search(len(records), func(i int) bool { return records[i].TimeMs >= fromMs })
	return append([]feedFundingRecord(nil), records[i:]...)
}

func (f *feedFunding) setErr() {
	switch {
	case f.ScalarErr != "" && f.RecordsErr != "":
		f.Err = "scalar: " + f.ScalarErr + "; records: " + f.RecordsErr
	case f.ScalarErr != "":
		f.Err = f.ScalarErr
	default:
		f.Err = f.RecordsErr
	}
}

func (f *feedFunding) freshRecords(at time.Time) bool {
	return f != nil && f.HasRecords && at.Sub(f.RecordsFetchedAt) <= feedFundingTTL
}

func (f *feedFunding) freshScalar(at time.Time) bool {
	return f != nil && f.HasScalar && at.Sub(f.ScalarFetchedAt) <= feedFundingTTL
}

func (f *feedFunding) provedMean(startMs, endMs int64) (float64, bool) {
	if f == nil || !f.HasRecords || f.RecordsFromMs > startMs || f.RecordsThroughMs < endMs {
		return 0, false
	}
	records := trimFundingRecords(f.Records, startMs)
	i := sort.Search(len(records), func(i int) bool { return records[i].TimeMs > endMs })
	return hlFundingMean(records[:i]), true
}

func fundingReaderStart(bars []feedBar, rows int) (int64, bool) {
	if len(bars) == 0 {
		return 0, false
	}
	i := max(len(bars)-rows-feedFundingRowMargin, 0)
	return bars[i].row().TimestampMs, true
}

func (o *marketFeedOwner) fundingRecordStarts(needs map[string]feedFundingNeed) map[string]int64 {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	out := make(map[string]int64, len(needs))
	for coin, need := range needs {
		if !need.Records {
			continue
		}
		start, known := int64(math.MaxInt64), false
		for _, r := range need.Readers {
			st := o.keys[r.Key]
			if st != nil {
				if ts, ok := fundingReaderStart(st.Bars, r.Rows); ok {
					known = true
					start = min(start, ts)
					continue
				}
			}
			if ms, ok := hlCandleIntervalMs(r.Key.Timeframe); ok {
				start = min(start, nowMs/ms*ms-int64(r.Rows+feedFundingRowMargin)*ms)
			}
		}
		if known && start > 0 {
			out[coin] = start
		}
	}
	return out
}

type feedFundingPlan struct {
	Needs      map[string]feedFundingNeed
	CoverageID string
	CutoffMs   int64
	Upcoming   []time.Time
	NextWeight int
	NextReads  int
}

type feedFundingItemKind int

const (
	feedFundingItemRecords feedFundingItemKind = iota
	feedFundingItemScalar
	feedFundingItemAccounting
)

type feedFundingItem struct {
	kind   feedFundingItemKind
	coin   string
	weight int
	reads  int
	loaded bool
	dueAt  int
	forced bool
}

type feedRoom struct {
	weight   int
	requests int
	spent    int
	sent     int
}

func (r *feedRoom) fits(weight, requests int) bool {
	return r.weight-r.spent >= weight && r.requests-r.sent >= requests
}

func (r *feedRoom) take(weight, requests int) {
	r.spent += weight
	r.sent += requests
}

func (o *marketFeedOwner) coverageMatches(id string) bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	return o.coverageMatchesLocked(id)
}

func (o *marketFeedOwner) coverageMatchesLocked(id string) bool {
	return id == "" || o.fundingCoverage == "" || id == o.fundingCoverage
}

func (o *marketFeedOwner) fundingEntry(coin string) feedFunding {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	if f := o.funding[coin]; f != nil {
		return *f
	}
	return feedFunding{Source: "rest"}
}

func (o *marketFeedOwner) commitFunding(coin string, records bool, seq uint64, fn func(f *feedFunding)) bool {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	need, ok := o.fundingNeeds[coin]
	if !ok || (records && !need.Records) || (!records && !need.Scalar) {
		return false
	}
	next := feedFunding{Source: "rest"}
	if f := o.funding[coin]; f != nil {
		next = *f
	}
	if records && next.recordsSeq != seq {
		return false
	}
	fn(&next)
	if records {
		o.fundingSeq++
		next.recordsSeq = o.fundingSeq
	}
	next.setErr()
	o.funding[coin] = &next
	return true
}

func (o *marketFeedOwner) EnsureFunding(ctx context.Context, plan feedFundingPlan) {
	base := withFeedReason(ctx, string(feedRestFunding))
	if !o.coverageMatches(plan.CoverageID) {
		o.logf("[feed-funding] mode=skip coverage=%s reason=%q", plan.CoverageID, "the published funding requirements differ from the ones this preparation captured")
		plan.Needs = nil
	}
	needs := o.requiredFunding(plan.Needs)
	starts := o.fundingRecordStarts(needs)
	now := o.now()
	if plan.CutoffMs == 0 {
		plan.CutoffMs = now.UTC().UnixMilli()
	}
	horizon := feedPrepareHorizon(ctx)
	room := feedFundingRoom(ctx)
	o.openFundingEntries(needs, starts, now)

	items := o.fundingItems(needs, starts, now, horizon, plan)
	accounting := o.accountingItems(now, horizon, plan)
	chosen := chooseFundingItems(items, accounting, plan, &room)
	var deferred []string
	for _, it := range append(append([]feedFundingItem(nil), items...), accounting...) {
		switch {
		case it.dueAt == 0 && !fundingItemChosen(chosen, it):
			deferred = append(deferred, it.label())
		case it.dueAt == 1 && !fundingItemChosen(chosen, it):
			plan.NextWeight += it.weight
			plan.NextReads += it.reads
		}
	}
	o.runFundingItems(base, chosen, needs, starts, now)
	o.loadFundingHistory(base, needs, starts, now, horizon, plan)
	if len(deferred) > 0 {
		sort.Strings(deferred)
		o.logf("[feed-funding] mode=deferred inputs=%s", joinFeedNames(deferred))
	}
}

func (it feedFundingItem) label() string {
	switch it.kind {
	case feedFundingItemAccounting:
		return "accounting_funding:" + it.coin
	case feedFundingItemScalar:
		return "funding_scalar:" + it.coin
	}
	return "funding:" + it.coin
}

func fundingItemChosen(chosen []feedFundingItem, it feedFundingItem) bool {
	for _, c := range chosen {
		if c.kind == it.kind && c.coin == it.coin {
			return true
		}
	}
	return false
}

func feedPrepareHorizon(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	left := time.Until(dl)
	if left > feedSealPrepareBudget {
		return feedSealPrepareBudget
	}
	if left < 0 {
		return 0
	}
	return left
}

func feedFundingRoom(ctx context.Context) feedRoom {
	weight, requests := feedLedgerFrom(ctx).roomUntil(feedHostOf(hlMainnetURL), nil, 0, 0)
	return feedRoom{weight: weight, requests: requests}
}

func (o *marketFeedOwner) requiredFunding(needs map[string]feedFundingNeed) map[string]feedFundingNeed {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make(map[string]feedFundingNeed, len(needs))
	for coin, need := range needs {
		required, ok := o.fundingNeeds[coin]
		if !ok {
			continue
		}
		need.Scalar = need.Scalar && required.Scalar
		need.Records = need.Records && required.Records
		if need.Scalar || need.Records {
			out[coin] = need
		}
	}
	return out
}

func (o *marketFeedOwner) openFundingEntries(needs map[string]feedFundingNeed, starts map[string]int64, now time.Time) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	for _, coin := range sortedFundingCoins(needs) {
		need := needs[coin]
		next := feedFunding{Source: "rest"}
		if f := o.funding[coin]; f != nil {
			next = *f
		}
		if next.openedAt.IsZero() {
			next.openedAt = now
		}
		if next.progressAt.IsZero() {
			next.progressAt = now
		}
		if need.Scalar && !next.HasScalar && next.ScalarErr == "" {
			next.ScalarErr = "the funding scalar is not fetched yet"
		}
		if need.Records && !next.HasRecords {
			_, known := starts[coin]
			switch {
			case !known:
				detail := fmt.Sprintf("funding records for %s wait for the first candle bar of its key; no fundingHistory read is sent without a frame start", coin)
				if !next.waitingForBars {
					o.logf("[feed] funding %s: %s", coin, detail)
				}
				next.RecordsErr, next.waitingForBars = detail, true
			case next.waitingForBars || next.RecordsErr == "":
				next.RecordsErr, next.waitingForBars = feedFundingLoading, false
			}
		}
		next.setErr()
		o.funding[coin] = &next
	}
}

func fundingDuePreparation(at time.Time, upcoming []time.Time, due func(prep time.Time, next bool) bool) int {
	if due(at, false) {
		return 0
	}
	for i, p := range upcoming {
		if due(p, true) {
			return i + 1
		}
	}
	return len(upcoming) + 1
}

func (o *marketFeedOwner) fundingItems(needs map[string]feedFundingNeed, starts map[string]int64, now time.Time, horizon time.Duration, plan feedFundingPlan) []feedFundingItem {
	tailRows, _ := hlFundingRowBound(0, hlFundingRecordStepMs)
	tailW, _ := hlInfoWeight("fundingHistory", tailRows)
	avgRows, _ := hlFundingRowBound(0, int64(hlFundingAvgWindowDays)*86_400_000)
	avgW, _ := hlInfoWeight("fundingHistory", avgRows)
	nowMs := now.UTC().UnixMilli()
	avgFrom := hlFundingAverageStart(nowMs)
	var items []feedFundingItem
	for _, coin := range sortedFundingCoins(needs) {
		need := needs[coin]
		f := o.fundingEntry(coin)
		tailNow := false
		if start, ok := starts[coin]; need.Records && ok && f.tailAnchored(start) {
			it := feedFundingItem{kind: feedFundingItemRecords, coin: coin, weight: tailW, reads: 1, loaded: true}
			it.forced = f.RecordsThroughMs < plan.CutoffMs
			it.dueAt = fundingDuePreparation(now, plan.Upcoming, func(prep time.Time, next bool) bool {
				return (it.forced && !next) || prep.Add(horizon).Sub(f.RecordsFetchedAt) > feedFundingTTL
			})
			tailNow = it.dueAt == 0 && f.RecordsFromMs <= avgFrom && start <= avgFrom
			items = append(items, it)
		}
		if !need.Scalar {
			continue
		}
		it := feedFundingItem{kind: feedFundingItemScalar, coin: coin, weight: avgW, reads: 1, loaded: f.HasScalar || now.Sub(f.openedAt) > feedFundingDeferralLimit}
		if _, proved := f.provedMean(avgFrom, nowMs); need.Records && (proved || tailNow) {
			it.weight, it.reads = 0, 0
		}
		it.dueAt = fundingDuePreparation(now, plan.Upcoming, func(prep time.Time, _ bool) bool {
			return !f.HasScalar || prep.Add(horizon).Sub(f.ScalarFetchedAt) > feedFundingTTL
		})
		items = append(items, it)
	}
	return items
}

func (f *feedFunding) tailAnchored(startMs int64) bool {
	n := len(f.Records)
	return f.HasRecords && n > 0 && f.Records[n-1].TimeMs >= startMs && f.Records[n-1].TimeMs >= f.RecordsFromMs
}

func fundingRolloverPreparation(plan feedFundingPlan) int {
	hour := plan.CutoffMs / hlFundingRecordStepMs
	for i, p := range plan.Upcoming {
		if p.UnixMilli()/hlFundingRecordStepMs > hour {
			return i + 1
		}
	}
	return 0
}

func (o *marketFeedOwner) accountingItems(now time.Time, horizon time.Duration, plan feedFundingPlan) []feedFundingItem {
	coins := o.accountingWork()
	if len(coins) == 0 {
		return nil
	}
	windowRows, _ := hlFundingRowBound(0, feedAccountingWindow.Milliseconds())
	overlapRows, _ := hlFundingRowBound(0, feedAccountingOverlap.Milliseconds()+2*feedAccountingCadence.Milliseconds())
	probeW, _ := hlInfoWeight("fundingHistory", 1)
	var items []feedFundingItem
	for _, coin := range coins {
		existing := o.accountingEntry(coin)
		if !accountingRefreshDue(existing, now) {
			continue
		}
		o.feedMu.Lock()
		dueSince, seen := o.accountingDue[coin]
		if !seen {
			dueSince = now
			o.accountingDue[coin] = now
		}
		o.feedMu.Unlock()
		rows := overlapRows
		if existing == nil || existing.ToMs == 0 {
			rows = windowRows
		}
		w, _ := hlInfoWeight("fundingHistory", rows)
		it := feedFundingItem{kind: feedFundingItemAccounting, coin: coin, weight: w + probeW, reads: 2, loaded: existing != nil && existing.ToMs > 0}
		limit := dueSince.Add(feedAccountingSlack)
		it.dueAt = fundingDuePreparation(now, plan.Upcoming, func(prep time.Time, _ bool) bool {
			return prep.Add(horizon).After(limit)
		})
		if len(plan.Upcoming) == 0 {
			it.dueAt = 0
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].dueAt < items[j].dueAt })
	return items
}

func chooseFundingItems(items, accounting []feedFundingItem, plan feedFundingPlan, room *feedRoom) []feedFundingItem {
	spread := len(plan.Upcoming) > 0
	ctxW, _ := hlInfoWeight("metaAndAssetCtxs", 0)
	var chosen []feedFundingItem
	weight := 0
	forced, scalars := false, false
	add := func(it feedFundingItem) {
		w, n := it.weight, it.reads
		if it.kind == feedFundingItemScalar && !scalars {
			scalars = true
			w, n = w+ctxW, n+1
		}
		weight += w
		room.take(w, n)
		chosen = append(chosen, it)
	}
	cost := func(it feedFundingItem) (int, int) {
		if it.kind == feedFundingItemScalar && !scalars {
			return it.weight + ctxW, it.reads + 1
		}
		return it.weight, it.reads
	}
	for _, it := range items {
		if it.dueAt != 0 {
			continue
		}
		if w, n := cost(it); !it.loaded && !room.fits(w, n) {
			continue
		}
		forced = forced || it.forced
		add(it)
	}
	if spread {
		target := fundingSpreadTarget(items, ctxW)
		rollover := fundingRolloverPreparation(plan)
		last := 0
		for _, it := range items {
			last = max(last, it.dueAt)
		}
		for at := 1; at <= last; at++ {
			load := 0
			for _, it := range items {
				if it.dueAt == at {
					load += it.weight
				}
			}
			for _, it := range items {
				if it.dueAt != at || fundingItemChosen(chosen, it) {
					continue
				}
				if at == rollover && it.kind == feedFundingItemRecords {
					continue
				}
				if load <= target && at != rollover {
					continue
				}
				w, n := cost(it)
				if (weight > 0 && weight+w > target) || !room.fits(w, n) {
					continue
				}
				load -= it.weight
				add(it)
			}
		}
	}
	if scalars {
		for _, it := range items {
			if it.kind == feedFundingItemScalar && it.weight == 0 && !fundingItemChosen(chosen, it) {
				add(it)
			}
		}
	}
	share := len(accounting)
	if spread && share > 0 {
		slots := 1
		for _, it := range accounting {
			slots = max(slots, it.dueAt)
		}
		share = (len(accounting) + slots - 1) / slots
	}
	taken := 0
	for _, it := range accounting {
		must := it.dueAt == 0
		if !must && (forced || taken >= share || !room.fits(it.weight, it.reads)) {
			continue
		}
		taken++
		add(it)
	}
	return chosen
}

func fundingSpreadTarget(items []feedFundingItem, ctxW int) int {
	total, direct, scalars, slots := 0, 0, 0, 1
	for _, it := range items {
		total += it.weight
		slots = max(slots, it.dueAt)
		if it.kind == feedFundingItemScalar {
			scalars++
			if it.weight > 0 {
				direct++
			}
		}
	}
	if scalars > 0 {
		total += ctxW * min(slots, max(direct, 1))
	}
	return (total + slots - 1) / slots
}

func (o *marketFeedOwner) runFundingItems(base context.Context, chosen []feedFundingItem, needs map[string]feedFundingNeed, starts map[string]int64, now time.Time) {
	var reads []func(context.Context)
	var scalarCoins []string
	var contexts map[string]hlFundingContext
	var contextErr error
	for _, it := range chosen {
		if it.kind == feedFundingItemScalar {
			scalarCoins = append(scalarCoins, it.coin)
		}
	}
	sort.Strings(scalarCoins)
	if len(scalarCoins) > 0 {
		reads = append(reads, func(jctx context.Context) {
			contexts, contextErr = fetchHyperliquidFundingContextsFn(withFeedInput(jctx, feedFundingContextInput))
			if contextErr == nil {
				feedLedgerFrom(jctx).weightInputValidated(feedFundingContextInput)
			}
		})
	}
	for _, kind := range []feedFundingItemKind{feedFundingItemRecords, feedFundingItemAccounting} {
		for _, it := range chosen {
			coin := it.coin
			switch {
			case it.kind != kind:
			case kind == feedFundingItemRecords:
				reads = append(reads, func(jctx context.Context) {
					o.refreshFundingTail(withFeedInput(jctx, "funding:"+coin), coin, starts[coin], now)
				})
			default:
				reads = append(reads, func(jctx context.Context) { o.refreshAccountingFunding(jctx, coin, now) })
			}
		}
	}
	runFeedOrdered(base, feedFundingWorkers, reads)

	var averages []func(context.Context)
	nowMs := now.UTC().UnixMilli()
	for _, coin := range scalarCoins {
		coin := coin
		rate, err := 0.0, contextErr
		if err == nil {
			rate, err = hlFundingContextRate(contexts, coin)
		}
		if err != nil {
			o.storeFundingScalar(base, coin, 0, 0, err, now)
			continue
		}
		entry := o.fundingEntry(coin)
		if avg, proved := entry.provedMean(hlFundingAverageStart(nowMs), nowMs); proved && needs[coin].Records {
			o.storeFundingScalar(base, coin, rate, avg, nil, now)
			o.logf("[feed-funding] coin=%s mode=scalar average=retained range=[%d,%d]", coin, hlFundingAverageStart(nowMs), nowMs)
			continue
		}
		averages = append(averages, func(jctx context.Context) {
			jctx = withFeedInput(jctx, "funding:"+coin)
			avg, avgErr := hlFundingAverage7d(jctx, coin, now)
			if avgErr == nil {
				o.logf("[feed-funding] coin=%s mode=scalar average=direct range=[%d,%d]", coin, hlFundingAverageStart(nowMs), nowMs)
			}
			o.storeFundingScalar(jctx, coin, rate, avg, avgErr, now)
		})
	}
	runFeedOrdered(base, feedFundingWorkers, averages)
}

func (o *marketFeedOwner) storeFundingScalar(ctx context.Context, coin string, rate, avg float64, err error, now time.Time) {
	weight := errors.Is(err, errFeedWeightRefused)
	committed := o.commitFunding(coin, false, 0, func(f *feedFunding) {
		if err != nil {
			f.ScalarErr = err.Error()
			f.scalarLimited = f.scalarLimited || feedRateLimitErr(err)
			f.scalarWeightHeld = f.scalarWeightHeld || weight
			return
		}
		f.Current, f.Avg7d, f.HasScalar = rate, avg, true
		f.ScalarErr, f.ScalarFetchedAt, f.FetchedAt = "", now, now
		f.scalarLimited, f.scalarWeightHeld = false, false
	})
	if !committed {
		return
	}
	if err == nil {
		o.validateFundingInput(ctx, coin)
		return
	}
	o.logf("[feed] funding %s: %v", coin, err)
	if !feedRateLimitErr(err) && !weight {
		o.raiseAlert(feedKeyFor(coin, ""), "funding_failed", err.Error())
	}
}

func (o *marketFeedOwner) validateFundingInput(ctx context.Context, coin string) {
	entry := o.fundingEntry(coin)
	if entry.scalarWeightHeld || entry.recordsWeightHeld {
		return
	}
	feedLedgerFrom(ctx).weightInputValidated("funding:" + coin)
}

func (o *marketFeedOwner) recordsFailed(coin string, seq uint64, err error, run *feedFundingRun, dropRecords bool) {
	weight := errors.Is(err, errFeedWeightRefused)
	committed := o.commitFunding(coin, true, seq, func(f *feedFunding) {
		f.RecordsErr = err.Error()
		f.waitingForBars = false
		f.recordsLimited = f.recordsLimited || feedRateLimitErr(err)
		f.recordsWeightHeld = f.recordsWeightHeld || weight
		f.run = run
		if dropRecords {
			f.Records, f.HasRecords = nil, false
			f.RecordsFromMs, f.RecordsToMs, f.RecordsThroughMs = 0, 0, 0
		}
	})
	if !committed {
		return
	}
	o.logf("[feed] funding %s: %v", coin, err)
	if !feedRateLimitErr(err) && !weight && !errors.Is(err, errFundingTailMismatch) {
		o.raiseAlert(feedKeyFor(coin, ""), "funding_failed", err.Error())
	}
}

func (o *marketFeedOwner) publishFundingRecords(ctx context.Context, coin string, seq uint64, run feedFundingRun, startMs int64, pending *feedFundingRun, now time.Time) {
	from := max(run.StartMs, startMs)
	records := trimFundingRecords(run.Records, from)
	if o.commitFunding(coin, true, seq, func(f *feedFunding) {
		f.Records, f.HasRecords, f.RecordsErr = records, true, ""
		f.RecordsFromMs, f.RecordsToMs, f.RecordsThroughMs = from, run.EndMs, feedFundingThrough(records, run.EndMs)
		f.RecordsFetchedAt, f.FetchedAt, f.progressAt = now, now, now
		f.waitingForBars, f.recordsLimited, f.recordsWeightHeld = false, false, false
		f.run = pending
	}) {
		o.validateFundingInput(ctx, coin)
	}
}

func (o *marketFeedOwner) refreshFundingTail(ctx context.Context, coin string, startMs int64, now time.Time) {
	entry := o.fundingEntry(coin)
	endMs := now.UTC().UnixMilli()
	run := feedFundingRun{StartMs: max(entry.RecordsFromMs, startMs), EndMs: endMs}
	run.Records = trimFundingRecords(entry.Records, run.StartMs)
	run = run.resumed(endMs)
	pending := entry.run
	if pending != nil && startMs >= entry.RecordsFromMs {
		pending = nil
	}
	anchor := run.boundary().TimeMs
	for read := 1; read <= feedFundingTailReads; read++ {
		next, complete, err := run.next(ctx, coin)
		if errors.Is(err, errFundingTailMismatch) {
			o.logf("[feed-funding] coin=%s mode=tail pages=%d refused: %v; reading the full range", coin, read, err)
			o.recordsFailed(coin, entry.recordsSeq, err, pending, true)
			return
		}
		if err != nil {
			o.recordsFailed(coin, entry.recordsSeq, err, pending, false)
			return
		}
		run = next
		if complete {
			o.logf("[feed-funding] coin=%s mode=tail range=[%d,%d] anchor=%d pages=%d records=%d", coin, run.StartMs, endMs, anchor, read, len(run.Records))
			o.publishFundingRecords(ctx, coin, entry.recordsSeq, run, startMs, pending, now)
			return
		}
	}
	if pending == nil {
		run.Pages = 0
		pending = &run
	}
	o.commitFunding(coin, true, entry.recordsSeq, func(f *feedFunding) {
		f.run = pending
		f.RecordsErr = fmt.Sprintf("%s: the tail from %d needs more pages", feedFundingLoading, anchor)
	})
}

func fundingLoadNeeded(entry feedFunding, startMs int64, at time.Time, cutoffMs int64) bool {
	if !entry.HasRecords || entry.RecordsFromMs > startMs || entry.run != nil {
		return true
	}
	if entry.tailAnchored(startMs) {
		return false
	}
	return at.Sub(entry.RecordsFetchedAt) > feedFundingTTL || entry.RecordsThroughMs < cutoffMs
}

type feedFundingLoad struct {
	coin      string
	run       feedFundingRun
	seq       uint64
	resumed   bool
	restarted bool
	done      bool
}

func (o *marketFeedOwner) loadFundingHistory(base context.Context, needs map[string]feedFundingNeed, starts map[string]int64, now time.Time, horizon time.Duration, plan feedFundingPlan) {
	endMs := now.UTC().UnixMilli()
	needed := func(coin string) bool {
		return fundingLoadNeeded(o.fundingEntry(coin), starts[coin], now.Add(horizon), plan.CutoffMs)
	}
	var coins []string
	for _, coin := range sortedFundingCoins(needs) {
		if _, ok := starts[coin]; ok && needs[coin].Records && needed(coin) {
			coins = append(coins, coin)
		}
	}
	if len(coins) == 0 {
		return
	}
	var loads []*feedFundingLoad
	for _, coin := range o.rotateFundingCoins(coins) {
		loads = append(loads, &feedFundingLoad{coin: coin})
	}
	ledger := feedLedgerFrom(base)
	host := feedHostOf(hlMainnetURL)
	slowest := feedFundingReadFloor
	var mu sync.Mutex
	for cursor := 0; ; {
		room := feedRoom{}
		room.weight, room.requests = ledger.roomUntil(host, plan.Upcoming, plan.NextWeight, plan.NextReads)
		var jobs []func(context.Context)
		for scanned := 0; scanned < len(loads) && len(jobs) < feedFundingWorkers; scanned++ {
			ld := loads[cursor%len(loads)]
			if ld.done {
				cursor++
				continue
			}
			entry := o.fundingEntry(ld.coin)
			ld.seq = entry.recordsSeq
			ld.run = feedFundingRun{StartMs: starts[ld.coin], EndMs: endMs}
			if entry.run != nil && entry.run.StartMs <= ld.run.StartMs {
				ld.run = *entry.run
				if !ld.resumed {
					ld.run = ld.run.resumed(endMs)
				}
			}
			w := ld.run.readWeight()
			starved := !room.fits(w, 1)
			if starved && (ld.resumed || len(jobs) > 0 || now.Sub(entry.progressAt) <= feedFundingDeferralLimit) || !feedTimeFor(base, slowest) {
				break
			}
			room.take(w, 1)
			ld.resumed = true
			cursor++
			o.markFundingCoinServed(ld.coin)
			jobs = append(jobs, func(jctx context.Context) {
				began := time.Now()
				o.loadFundingPage(withFeedInput(jctx, "funding:"+ld.coin), ld, starts[ld.coin], now)
				mu.Lock()
				defer mu.Unlock()
				slowest = max(slowest, time.Since(began))
			})
		}
		if len(jobs) == 0 {
			break
		}
		runFeedOrdered(base, feedFundingWorkers, jobs)
	}
	var loading []string
	for _, coin := range coins {
		if needed(coin) {
			loading = append(loading, coin)
		}
	}
	if len(loading) > 0 {
		o.logf("[feed-funding] mode=loading coins=%s", joinFeedNames(loading))
	}
}

func (o *marketFeedOwner) loadFundingPage(ctx context.Context, ld *feedFundingLoad, startMs int64, now time.Time) {
	next, complete, err := ld.run.next(ctx, ld.coin)
	switch {
	case errors.Is(err, errFundingTailMismatch):
		o.logf("[feed-funding] coin=%s mode=load refused: %v; restarting the range", ld.coin, err)
		o.recordsFailed(ld.coin, ld.seq, err, nil, false)
		ld.done = ld.restarted
		ld.restarted = true
	case err != nil:
		keep := &ld.run
		if ld.run.Pages >= hlFundingMaxRangePasses || len(ld.run.Records) == 0 {
			keep = nil
		}
		o.recordsFailed(ld.coin, ld.seq, err, keep, false)
		ld.done = true
	case complete:
		o.logf("[feed-funding] coin=%s mode=full range=[%d,%d] pages=%d records=%d", ld.coin, next.StartMs, next.EndMs, next.Pages, len(next.Records))
		o.publishFundingRecords(ctx, ld.coin, ld.seq, next, startMs, nil, now)
		ld.done = true
	default:
		detail := fmt.Sprintf("%s: %d page(s) and %d record(s) of the range [%d, %d] are verified through %d", feedFundingLoading, next.Pages, len(next.Records), next.StartMs, next.EndMs, next.cursor()-1)
		if !o.commitFunding(ld.coin, true, ld.seq, func(f *feedFunding) {
			f.run, f.waitingForBars, f.progressAt = &next, false, now
			if !f.HasRecords {
				f.RecordsErr = detail
			}
		}) {
			ld.done = true
			return
		}
		o.logf("[feed-funding] coin=%s mode=checkpoint %s", ld.coin, detail)
	}
}

func feedTimeFor(ctx context.Context, need time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > need
}

func (o *marketFeedOwner) rotateFundingCoins(coins []string) []string {
	o.feedMu.Lock()
	last := o.fundingLastServed
	o.feedMu.Unlock()
	i := sort.SearchStrings(coins, last)
	if i < len(coins) && coins[i] == last {
		i++
	}
	if i >= len(coins) {
		i = 0
	}
	return append(append([]string(nil), coins[i:]...), coins[:i]...)
}

func (o *marketFeedOwner) markFundingCoinServed(coin string) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.fundingLastServed = coin
}

func (o *marketFeedOwner) FundingLoading() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := []string{}
	for coin, need := range o.fundingNeeds {
		f := o.funding[coin]
		if need.Records && (f == nil || !f.HasRecords || f.run != nil) {
			out = append(out, "funding:"+coin)
		}
		if need.Scalar && (f == nil || !f.HasScalar) {
			out = append(out, "funding_scalar:"+coin)
		}
	}
	for coin := range o.accountingCoins {
		if a := o.accountingFunding[coin]; a == nil || a.ToMs == 0 {
			out = append(out, "accounting_funding:"+coin)
		}
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) retireFundingLocked(needs map[string]feedFundingNeed) {
	for coin, f := range o.funding {
		need, ok := needs[coin]
		if !ok || (!need.Scalar && !need.Records) {
			delete(o.funding, coin)
			continue
		}
		next := *f
		if !need.Records {
			next.Records, next.HasRecords, next.RecordsErr, next.run = nil, false, "", nil
			next.RecordsFromMs, next.RecordsToMs, next.RecordsThroughMs = 0, 0, 0
			next.recordsWeightHeld, next.recordsLimited, next.waitingForBars = false, false, false
			o.fundingSeq++
			next.recordsSeq = o.fundingSeq
		}
		if !need.Scalar {
			next.HasScalar, next.ScalarErr = false, ""
			next.scalarWeightHeld, next.scalarLimited = false, false
		}
		next.setErr()
		o.funding[coin] = &next
	}
}

func (o *marketFeedOwner) freezeFundingLocked(coin string, need feedFundingNeed, now time.Time, coverageOK bool) (feedFunding, bool) {
	f := o.funding[coin]
	if f == nil {
		return feedFunding{}, false
	}
	cp := *f
	cp.run = nil
	if !need.Scalar || !cp.freshScalar(now) {
		if need.Scalar && cp.HasScalar && cp.ScalarErr == "" {
			cp.ScalarErr = fmt.Sprintf("the newest funding scalar was read at %d, over the %s freshness limit", feedTimeMs(cp.ScalarFetchedAt), feedFundingTTL)
		}
		cp.HasScalar, cp.Current, cp.Avg7d = false, 0, 0
	} else {
		cp.ScalarErr = ""
	}
	if !need.Scalar {
		cp.ScalarErr = ""
	}
	cp.RecordsContract = 0
	if !need.Records {
		cp.RecordsErr, cp.HasRecords = "", false
	} else {
		cp.RecordsContract = feedFundingRecordsContract
		switch {
		case !coverageOK:
			cp.HasRecords = false
			cp.RecordsErr = feedFundingCoverageChanged
		case !cp.HasRecords:
			if cp.RecordsErr == "" {
				cp.RecordsErr = feedFundingLoading
			}
		case !cp.freshRecords(now):
			cp.HasRecords = false
			if cp.RecordsErr == "" {
				cp.RecordsErr = fmt.Sprintf("the newest proved funding read ended at %d, over the %s freshness limit", cp.RecordsToMs, feedFundingTTL)
			}
		default:
			cp.RecordsErr = ""
		}
	}
	if cp.HasRecords {
		cp.Records = append([]feedFundingRecord(nil), f.Records...)
	} else {
		cp.Records = nil
		cp.RecordsFromMs, cp.RecordsToMs, cp.RecordsThroughMs = 0, 0, 0
	}
	cp.Err = ""
	if cp.RecordsContract == 0 {
		cp.Err = cp.ScalarErr
	}
	return cp, true
}

func (s *marketSnapshot) stripFundingCoverage(reason string) bool {
	stripped := false
	for coin, f := range s.funding {
		if f.RecordsContract == 0 {
			continue
		}
		f.HasRecords, f.Records, f.RecordsErr = false, nil, reason
		f.RecordsFromMs, f.RecordsToMs, f.RecordsThroughMs = 0, 0, 0
		s.funding[coin] = f
		stripped = true
	}
	return stripped
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
	case scalar && f.ScalarErr != "":
		return true, fmt.Sprintf("funding scalar fetch for %s failed: %s", coin, f.ScalarErr)
	case scalar && !f.HasScalar:
		return true, fmt.Sprintf("sealed funding for %s carries no current rate", coin)
	case records && f.RecordsContract != feedFundingRecordsContract:
		return true, fmt.Sprintf("sealed funding for %s carries no record coverage proof (contract %d, want %d); the feed must run the build of this consumer", coin, f.RecordsContract, feedFundingRecordsContract)
	case records && f.RecordsErr != "":
		return true, fmt.Sprintf("funding records fetch for %s failed: %s", coin, f.RecordsErr)
	case records && !f.HasRecords:
		return true, fmt.Sprintf("sealed funding for %s carries no history", coin)
	}
	return false, ""
}

func (s *marketSnapshot) fundingReaderHold(entry feedStrategyRequirement) (bool, string) {
	if held, why := s.fundingHold(entry.Coin, entry.FundingScalar, entry.FundingRecords); held || !entry.FundingRecords {
		return held, why
	}
	f := s.funding[entry.Coin]
	if frame := s.keys[entry.Signal]; frame != nil && len(frame.Bars) > 0 {
		first := frame.Bars[max(len(frame.Bars)-entry.SignalLookback, 0)].row().TimestampMs
		if first < f.RecordsFromMs {
			return true, fmt.Sprintf("the selected %s frame starts at %d, before the proved funding range of %s that starts at %d", entry.Signal.PayloadID(), first, entry.Coin, f.RecordsFromMs)
		}
	}
	if age := s.SealedAt.UnixMilli() - f.RecordsToMs; age > feedFundingTTL.Milliseconds() {
		return true, fmt.Sprintf("the proved funding read of %s ended %dms before the seal, over the %s limit", entry.Coin, age, feedFundingTTL)
	}
	if intervalMs, ok := hlCandleIntervalMs(entry.Signal.Timeframe); entry.ClosedBar && ok && intervalMs > 0 {
		boundary := s.decisionCutoff().UnixMilli() / intervalMs * intervalMs
		if f.RecordsThroughMs < boundary {
			return true, fmt.Sprintf("the funding records of %s are proved through %d, before the closed decision boundary %d", entry.Coin, f.RecordsThroughMs, boundary)
		}
	}
	return false, ""
}
