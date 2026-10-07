package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	feedAccountingWindow    = 7 * 24 * time.Hour
	feedAccountingRefresh   = 60 * time.Second
	feedAccountingOverlap   = 3 * time.Hour
	feedAccountingMaxPasses = 4

	feedAccountingNotFetched = "accounting funding not fetched yet"
)

type feedAccountingFunding struct {
	Records   []feedFundingRecord
	FromMs    int64
	ToMs      int64
	FetchedAt time.Time
	Err       string
}

type feedSealAccountingFunding struct {
	Coin        string              `json:"coin"`
	FromMs      int64               `json:"from_ms"`
	ToMs        int64               `json:"to_ms"`
	Records     []feedFundingRecord `json:"records"`
	FetchedAtMs int64               `json:"fetched_at_ms"`
	Error       string              `json:"error"`
}

func (f feedAccountingFunding) clone() feedAccountingFunding {
	f.Records = append([]feedFundingRecord(nil), f.Records...)
	return f
}

func (f feedAccountingFunding) coverage(coin string) feedFundingCoverage {
	return feedFundingCoverage{Coin: coin, FromMs: f.FromMs, ToMs: f.ToMs, Records: append([]feedFundingRecord(nil), f.Records...)}
}

func (o *marketFeedOwner) applyAccountingCoinsLocked(coins []string) {
	next := make(map[string]bool, len(coins))
	for _, c := range coins {
		next[c] = true
	}
	o.accountingCoins = next
	for c := range o.accountingFunding {
		if !next[c] {
			delete(o.accountingFunding, c)
		}
	}
}

func (o *marketFeedOwner) accountingWork() []string {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make([]string, 0, len(o.accountingCoins))
	for c := range o.accountingCoins {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (o *marketFeedOwner) EnsureAccountingFunding(ctx context.Context) {
	coins := o.accountingWork()
	if len(coins) == 0 {
		return
	}
	ctx = withFeedReason(ctx, string(feedRestAccountingFunding))
	now := o.now()
	windowStart := now.UTC().Add(-feedAccountingWindow).UnixMilli()
	for _, coin := range coins {
		o.feedMu.Lock()
		var existing *feedAccountingFunding
		if cur := o.accountingFunding[coin]; cur != nil {
			cp := cur.clone()
			existing = &cp
		}
		o.feedMu.Unlock()
		if existing != nil && now.Sub(existing.FetchedAt) < feedAccountingRefresh {
			continue
		}
		start := windowStart
		if existing != nil && existing.ToMs > 0 {
			if s := existing.ToMs - feedAccountingOverlap.Milliseconds(); s > start {
				start = s
			}
		}
		cov, err := hlFundingRecordsCoverage(ctx, coin, start, now, feedAccountingMaxPasses)
		var next feedAccountingFunding
		if err == nil {
			next, err = mergeAccountingCoverage(existing, cov, windowStart)
		}
		if err != nil {
			if existing != nil {
				next = *existing
			} else {
				next = feedAccountingFunding{}
			}
			next.Err = err.Error()
			o.logf("[feed] accounting funding %s: %s", coin, next.Err)
			o.raiseAlert(feedKeyFor(coin, ""), "accounting_funding_failed", next.Err)
		} else {
			next.Err = ""
		}
		next.FetchedAt = now
		o.feedMu.Lock()
		if o.accountingCoins[coin] {
			o.accountingFunding[coin] = &next
		}
		o.feedMu.Unlock()
	}
}

func mergeAccountingCoverage(existing *feedAccountingFunding, cov feedFundingCoverage, windowStart int64) (feedAccountingFunding, error) {
	if err := validateFundingCoverageRecords(cov.Coin, cov.FromMs, cov.ToMs, cov.Records); err != nil {
		return feedAccountingFunding{}, err
	}
	var out feedAccountingFunding
	if existing == nil || len(existing.Records) == 0 || cov.FromMs > existing.ToMs+1 {
		out.FromMs = cov.FromMs
		out.Records = append([]feedFundingRecord(nil), cov.Records...)
	} else {
		merged, err := mergeFundingRecordSets(cov.Coin, existing.Records, cov)
		if err != nil {
			return feedAccountingFunding{}, err
		}
		out.FromMs = existing.FromMs
		if cov.FromMs < out.FromMs {
			out.FromMs = cov.FromMs
		}
		out.Records = merged
	}
	if out.FromMs < windowStart {
		out.FromMs = windowStart
	}
	kept := out.Records[:0]
	for _, r := range out.Records {
		if r.TimeMs >= out.FromMs {
			kept = append(kept, r)
		}
	}
	out.Records = append([]feedFundingRecord(nil), kept...)
	if n := len(out.Records); n > 0 {
		out.ToMs = out.Records[n-1].TimeMs
	}
	return out, nil
}

func mergeFundingRecordSets(coin string, prior []feedFundingRecord, cov feedFundingCoverage) ([]feedFundingRecord, error) {
	fresh := make(map[int64]float64, len(cov.Records))
	for _, r := range cov.Records {
		fresh[r.TimeMs] = r.Rate
	}
	byTime := make(map[int64]float64, len(prior)+len(cov.Records))
	for _, r := range prior {
		if rate, ok := fresh[r.TimeMs]; ok {
			if rate != r.Rate {
				return nil, fmt.Errorf("funding %s record at %d changed rate from %v to %v", coin, r.TimeMs, r.Rate, rate)
			}
		} else if cov.ToMs > 0 && r.TimeMs >= cov.FromMs && r.TimeMs <= cov.ToMs {
			return nil, fmt.Errorf("funding %s record at %d is missing from a later fetch that covers it", coin, r.TimeMs)
		}
		byTime[r.TimeMs] = r.Rate
	}
	for t, rate := range fresh {
		byTime[t] = rate
	}
	out := make([]feedFundingRecord, 0, len(byTime))
	for t, rate := range byTime {
		out = append(out, feedFundingRecord{TimeMs: t, Rate: rate})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}

func validateFundingCoverageRecords(coin string, fromMs, toMs int64, records []feedFundingRecord) error {
	if fromMs < 0 || toMs < 0 {
		return fmt.Errorf("funding coverage %s has a negative bound (%d, %d)", coin, fromMs, toMs)
	}
	if len(records) == 0 {
		if toMs != 0 {
			return fmt.Errorf("funding coverage %s has to_ms %d with no records", coin, toMs)
		}
		return nil
	}
	var last int64
	for i, r := range records {
		if math.IsNaN(r.Rate) || math.IsInf(r.Rate, 0) {
			return fmt.Errorf("funding coverage %s record %d carries a non-finite rate", coin, i)
		}
		if r.TimeMs < fromMs || r.TimeMs <= 0 {
			return fmt.Errorf("funding coverage %s record %d time %d is before from_ms %d", coin, i, r.TimeMs, fromMs)
		}
		if i > 0 && r.TimeMs <= last {
			return fmt.Errorf("funding coverage %s records are not strictly increasing at record %d", coin, i)
		}
		last = r.TimeMs
	}
	if toMs != last {
		return fmt.Errorf("funding coverage %s to_ms %d is not its last record time %d", coin, toMs, last)
	}
	return nil
}

func (o *marketFeedOwner) freezeAccountingFundingLocked(snap *marketSnapshot, coins []string) {
	if len(coins) == 0 {
		return
	}
	snap.accountingFunding = make(map[string]feedAccountingFunding, len(coins))
	for _, coin := range coins {
		if cur := o.accountingFunding[coin]; cur != nil {
			snap.accountingFunding[coin] = cur.clone()
			continue
		}
		snap.accountingFunding[coin] = feedAccountingFunding{Err: feedAccountingNotFetched}
	}
}

func sealAccountingFunding(entries map[string]feedAccountingFunding) []feedSealAccountingFunding {
	if len(entries) == 0 {
		return nil
	}
	coins := make([]string, 0, len(entries))
	for c := range entries {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	out := make([]feedSealAccountingFunding, 0, len(coins))
	for _, c := range coins {
		f := entries[c]
		out = append(out, feedSealAccountingFunding{
			Coin:        c,
			FromMs:      f.FromMs,
			ToMs:        f.ToMs,
			Records:     append([]feedFundingRecord{}, f.Records...),
			FetchedAtMs: feedTimeMs(f.FetchedAt),
			Error:       f.Err,
		})
	}
	return out
}

func validateFeedSealAccountingFunding(entries []feedSealAccountingFunding) error {
	for i, f := range entries {
		if f.Coin == "" {
			return fmt.Errorf("seal accounting funding entry %d has no coin", i)
		}
		if i > 0 && entries[i-1].Coin >= f.Coin {
			return fmt.Errorf("seal accounting funding is not strictly ordered at %s", f.Coin)
		}
		if f.FetchedAtMs < 0 {
			return fmt.Errorf("seal accounting funding %s has fetched_at_ms %d", f.Coin, f.FetchedAtMs)
		}
		if err := validateFundingCoverageRecords(f.Coin, f.FromMs, f.ToMs, f.Records); err != nil {
			return fmt.Errorf("seal %w", err)
		}
	}
	return nil
}

func snapshotAccountingFunding(entries []feedSealAccountingFunding) map[string]feedAccountingFunding {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]feedAccountingFunding, len(entries))
	for _, f := range entries {
		out[f.Coin] = feedAccountingFunding{
			Records:   append([]feedFundingRecord(nil), f.Records...),
			FromMs:    f.FromMs,
			ToMs:      f.ToMs,
			FetchedAt: feedMsTime(f.FetchedAtMs),
			Err:       f.Error,
		}
	}
	return out
}

func (s *marketSnapshot) accountingCoverage(coin string) (feedFundingCoverage, string, bool) {
	if s == nil || s.accountingFunding == nil {
		return feedFundingCoverage{}, "no sealed accounting funding", false
	}
	f, ok := s.accountingFunding[coin]
	if !ok {
		return feedFundingCoverage{}, fmt.Sprintf("accounting funding for %s is not in the seal", coin), false
	}
	return f.coverage(coin), f.Err, true
}
