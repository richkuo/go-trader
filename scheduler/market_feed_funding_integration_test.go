package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hlFundingRecordsSince(ctx context.Context, coin string, startMs int64, now time.Time) ([]feedFundingRecord, error) {
	run := feedFundingRun{StartMs: startMs, EndMs: now.UTC().UnixMilli()}
	for {
		next, complete, err := run.next(ctx, coin)
		if err != nil {
			return nil, err
		}
		if complete {
			return next.Records, nil
		}
		run = next
	}
}

type fcFixture struct {
	*rdFixture
	consumers []feedConsumer
	byID      map[string]StrategyConfig
}

func fcStrategies() (live, paper []StrategyConfig) {
	for _, coin := range rdCoins {
		live = append(live,
			rdStrategy("live-"+coin+"-5m", "sma_crossover", coin, "5m", 300, false, false, true),
			rdStrategy("live-"+coin+"-15m", "rsi", coin, "15m", 300, false, false, true))
		paper = append(paper,
			rdStrategy("paper-"+coin+"-30m-skew", "funding_skew", coin, "30m", 300, false, false, true),
			rdStrategy("paper-"+coin+"-1h-skew", "funding_skew", coin, "1h", 300, true, false, true),
			rdStrategy("paper-"+coin+"-30m-neutral", "delta_neutral_funding", coin, "30m", 300, false, false, true))
		if coin != bitcoinLeadLagLeaderCoin {
			paper = append(paper, rdStrategy("paper-"+coin+"-4h-lag", bitcoinLeadLagStrategyName, coin, "4h", 300, false, false, true))
		}
	}
	return live, paper
}

func fcWorkload(t *testing.T) []feedConsumer {
	t.Helper()
	live, paper := fcStrategies()
	return []feedConsumer{rdConsumer(t, "/live.json", live), rdConsumer(t, "/paper.json", paper)}
}

func newFCFixture(t *testing.T, start time.Time, build func() []feedConsumer) *fcFixture {
	t.Helper()
	rd, consumers := newRDFixture(t, start, build)
	f := &fcFixture{rdFixture: rd, consumers: consumers, byID: map[string]StrategyConfig{}}
	live, paper := fcStrategies()
	for _, sc := range append(live, paper...) {
		f.byID[sc.ID] = sc
	}
	return f
}

func (f *fcFixture) enforce(perMinute, startup, weight, weightStartup, reserve int) {
	f.ledger = newFeedRequestLedger(&feedBudgetConfig{PerMinute: perMinute, Startup: startup,
		WeightPerMinute: &weight, WeightStartup: &weightStartup, WeightReserve: &reserve}, true, f.clock.Now)
	f.ledger.logf = f.logf
	f.ctx = withFeedLedger(context.Background(), f.ledger)
	f.sealer.ledger = f.ledger
}

func (f *fcFixture) truth(coin string, fromMs, toMs int64) []feedFundingRecord {
	var out []feedFundingRecord
	for h := (fromMs + hlFundingRecordStepMs - 1) / hlFundingRecordStepMs * hlFundingRecordStepMs; h <= toMs; h += hlFundingRecordStepMs {
		out = append(out, feedFundingRecord{TimeMs: h, Rate: rdFundingRate(coin, h)})
	}
	return out
}

func (f *fcFixture) context(doc *feedSealDoc, consumer int) *marketFeedContext {
	return &marketFeedContext{Enabled: true, Requirements: f.consumers[consumer].Req, Snapshot: doc.snapshot(), Interval: 300}
}

func (f *fcFixture) requireUsable(doc *feedSealDoc) {
	f.t.Helper()
	ready := 0
	for _, k := range doc.Keys {
		if k.Readiness.Ready {
			ready++
		}
	}
	if ready != len(f.union.Order) {
		f.t.Fatalf("deadline %d sealed %d of %d keys ready; logs %v", doc.Key, ready, len(f.union.Order), f.logsContaining("refresh for deadline"))
	}
	if len(doc.Mids) != len(rdCoins) {
		f.t.Fatalf("deadline %d sealed %d mids, want %d", doc.Key, len(doc.Mids), len(rdCoins))
	}
	for _, m := range doc.Mids {
		if age := time.Duration(doc.SealedAtMs-m.RecvAtMs) * time.Millisecond; age > feedRESTMidsHold || m.RecvAtMs < doc.Key*1000 {
			f.t.Fatalf("deadline %d mid %s is %s old at the seal, want a read of this preparation within %s", doc.Key, m.Coin, age, feedRESTMidsHold)
		}
	}
	for i := range f.consumers {
		fc := f.context(doc, i)
		for id := range fc.Requirements.Strategies {
			if hold := fc.holdFor(f.byID[id]); hold.Held {
				f.t.Fatalf("deadline %d holds %s: %s", doc.Key, id, hold.Reason)
			}
		}
	}
	for _, a := range doc.AccountingFunding {
		if a.Error != "" || a.ToMs == 0 {
			f.t.Fatalf("deadline %d accounting %+v is not usable", doc.Key, a.Coin)
		}
	}
}

func TestFeedRESTSteadyBudget(t *testing.T) {
	hour := rdBase()
	f := newFCFixture(t, time.Unix(hour-1800, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
	if len(f.union.Order) != 25 || len(f.union.Funding) != 5 || len(f.union.AccountingCoins) != 5 {
		t.Fatalf("fixture = %d keys, %d funding coins, %d accounting coins; want 25, 5 and 5", len(f.union.Order), len(f.union.Funding), len(f.union.AccountingCoins))
	}
	f.venue.set(func(v *rdVenue) {
		v.delay = func(rdRequest) time.Duration { f.clock.Advance(200 * time.Millisecond); return 0 }
	})
	run := func(from, to int64, verify bool) (weightPeak, readPeak int) {
		for d := from; d < to; d += 75 {
			doc, _ := f.seal(d)
			st := f.ledger.status()
			weightPeak = max(weightPeak, st.Weight.DemandPeak, st.Weight.Charged+st.Weight.Pending+st.Weight.InFlight)
			readPeak = max(readPeak, st.DemandPeak, st.WindowUsed)
			if verify {
				f.requireUsable(doc)
			}
		}
		return weightPeak, readPeak
	}
	f.enforce(500, 500, 900, 100, 200)
	run(hour-1500, hour-600, false)
	if st := f.ledger.status(); st.Totals.Refused != 0 || st.Weight.Totals.Refused != 0 {
		t.Fatalf("warm-up refused %d requests and %d weight reads: %+v", st.Totals.Refused, st.Weight.Totals.Refused, st.Weight.Totals.RefusedByReason)
	}
	weightPeak, readPeak := run(hour-600, hour+900, true)
	t.Logf("measured peaks: weight=%d reads=%d", weightPeak, readPeak)
	if weightPeak > 666 {
		t.Fatalf("weight peak = %d, want at most 666 so the 1.5 calibration margin fits reserve 200", weightPeak)
	}
	derivedWeight, derivedReads := (weightPeak*3+1)/2, (readPeak*3+1)/2
	if derivedWeight > hlRESTWeightCeiling-200 {
		t.Fatalf("derived weight_per_minute %d is above the %d allocation limit", derivedWeight, hlRESTWeightCeiling-200)
	}
	for _, budget := range []struct{ reads, weight, startup int }{{derivedReads, 900, 100}, {derivedReads, derivedWeight, min(100, hlRESTWeightCeiling-200-derivedWeight)}} {
		f.enforce(budget.reads, 1, budget.weight, budget.startup, 200)
		hour += 3600
		run(hour-600, hour-300, false)
		run(hour-300, hour+600, true)
		if st := f.ledger.status(); st.Totals.Refused != 0 || st.Weight.Totals.Refused != 0 || st.Totals.Paused != 0 {
			t.Fatalf("budget %+v refused %d requests and %d weight reads: %+v", budget, st.Totals.Refused, st.Weight.Totals.Refused, st.Weight.Totals.RefusedByReason)
		}
	}
}

func (f *fcFixture) widen(timeframe string, rows int) {
	f.t.Helper()
	for i := range f.consumers {
		req := &f.consumers[i].Req
		for id, entry := range req.Strategies {
			if !entry.FundingRecords || entry.Signal.Timeframe != timeframe {
				continue
			}
			entry.SignalLookback = rows
			req.Strategies[id] = entry
			req.Keys[entry.Signal] = rows
			req.Funding[entry.Coin] = req.Funding[entry.Coin].merge(entry.fundingNeed())
		}
	}
}

func (f *fcFixture) reinstall() {
	f.t.Helper()
	ctx := f.ctx
	f.ctx = withFeedLedger(context.Background(), newFeedRequestLedger(nil, false, f.clock.Now))
	f.install(unionFeedRequirements(f.consumers), unionFeedCadences(f.consumers))
	f.ctx = ctx
}

func (f *fcFixture) fundingOf(doc *feedSealDoc, coin string) feedSealFunding {
	f.t.Helper()
	for _, e := range doc.Funding {
		if e.Coin == coin {
			return e
		}
	}
	f.t.Fatalf("seal %d carries no funding entry for %s", doc.Key, coin)
	return feedSealFunding{}
}

func (f *fcFixture) checkpoint(coin string) *feedFundingRun {
	f.owner.feedMu.Lock()
	defer f.owner.feedMu.Unlock()
	if e := f.owner.funding[coin]; e != nil && e.run != nil {
		run := *e.run
		return &run
	}
	return nil
}

func (f *fcFixture) requireTruth(doc *feedSealDoc, coin string) {
	f.t.Helper()
	e := f.fundingOf(doc, coin)
	if !e.HasRecords || e.RecordsError != "" {
		f.t.Fatalf("deadline %d %s records = %q, want complete records", doc.Key, coin, e.RecordsError)
	}
	want := f.truth(coin, e.RecordsFromMs, e.RecordsToMs)
	if len(e.Records) != len(want) {
		f.t.Fatalf("deadline %d %s carries %d records in [%d, %d], want %d", doc.Key, coin, len(e.Records), e.RecordsFromMs, e.RecordsToMs, len(want))
	}
	for i := range want {
		if e.Records[i] != want[i] {
			f.t.Fatalf("deadline %d %s record %d = %+v, want %+v", doc.Key, coin, i, e.Records[i], want[i])
		}
	}
}

func fundingReads(reqs []rdRequest, coin string) []rdRequest {
	var out []rdRequest
	for _, r := range reqs {
		if r.Type == "fundingHistory" && (coin == "" || r.Coin == coin) {
			out = append(out, r)
		}
	}
	return out
}

const fcDeepRows = 3010

func newFCDeepFixture(t *testing.T, start time.Time) *fcFixture {
	t.Helper()
	f := newFCFixture(t, start, func() []feedConsumer {
		consumers := fcWorkload(t)
		(&fcFixture{rdFixture: &rdFixture{t: t}, consumers: consumers}).widen("1h", fcDeepRows)
		return consumers
	})
	f.enforce(60, 60, 900, 100, 200)
	return f
}

func TestFeedFundingColdLoadConverges(t *testing.T) {
	t0 := rdBase()
	f := newFCDeepFixture(t, time.Unix(t0-2000, 0).UTC())
	f.venue.set(func(v *rdVenue) {
		v.delay = func(rdRequest) time.Duration { f.clock.Advance(150 * time.Millisecond); return 0 }
	})
	pages := map[string][]rdRequest{}
	complete := map[string]int64{}
	var last *feedSealDoc
	for d := t0; d < t0+1800 && len(complete) < len(rdCoins); d += 75 {
		mark := f.venue.mark()
		doc, _ := f.seal(d)
		last = doc
		ready := 0
		for _, k := range doc.Keys {
			if k.Readiness.Ready {
				ready++
			}
		}
		if ready != 25 || len(doc.Mids) != len(rdCoins) {
			t.Fatalf("deadline %d during the cold load sealed %d ready keys and %d mids, want 25 and %d", d, ready, len(doc.Mids), len(rdCoins))
		}
		for _, coin := range rdCoins {
			for _, r := range fundingReads(f.venue.since(mark, ""), coin) {
				if r.End-r.Start > 8*86_400_000 || r.Rows == hlFundingRowCap {
					pages[coin] = append(pages[coin], r)
				}
			}
			e := f.fundingOf(doc, coin)
			if loading := strings.Join(f.owner.FundingLoading(), ",") + ","; !e.HasRecords && !strings.Contains(loading, "funding:"+coin+",") {
				t.Fatalf("deadline %d loading inputs %s do not name %s", d, loading, coin)
			}
			if complete[coin] == 0 && e.HasRecords {
				complete[coin] = d
			}
			if complete[coin] == 0 && (len(e.Records) != 0 || e.RecordsFromMs != 0 || !strings.Contains(e.RecordsError, feedFundingLoading)) {
				t.Fatalf("deadline %d seals %s with %d partial records, coverage from %d and reason %q", d, coin, len(e.Records), e.RecordsFromMs, e.RecordsError)
			}
			if complete[coin] == 0 {
				fc := f.context(doc, 1)
				if hold := fc.holdFor(f.byID["paper-"+coin+"-1h-skew"]); !hold.Held {
					t.Fatalf("deadline %d evaluates %s on a partial history", d, coin)
				}
				if hold := fc.holdFor(f.byID["paper-"+coin+"-30m-neutral"]); d > t0+300 && hold.Held {
					t.Fatalf("deadline %d holds the scalar reader of %s for a record load: %s", d, coin, hold.Reason)
				}
			}
		}
		if d == t0+75*8 {
			for _, a := range doc.AccountingFunding {
				if a.ToMs == 0 {
					t.Fatalf("accounting funding %s is not loaded %ds into the cold load", a.Coin, d-t0)
				}
			}
		}
	}
	if len(complete) != len(rdCoins) {
		t.Fatalf("complete coins = %v after %s, want all; logs %v", complete, time.Duration(last.Key-t0)*time.Second, f.logsContaining("mode=loading"))
	}
	first, final := int64(0), int64(0)
	for coin, reads := range pages {
		if len(reads) < 6 {
			t.Fatalf("%s loaded in %d deep reads, want the 3011-hour range in bounded pages", coin, len(reads))
		}
		for i := 1; i < len(reads); i++ {
			prevLast := reads[i-1].Start + int64(reads[i-1].Rows-1)*hlFundingRecordStepMs
			if reads[i].Start < (prevLast/hlFundingRecordStepMs)*hlFundingRecordStepMs {
				t.Fatalf("%s page %d starts at %d and rereads completed records through %d", coin, i, reads[i].Start, prevLast)
			}
		}
		if first == 0 || complete[coin] < first {
			first = complete[coin]
		}
		final = max(final, complete[coin])
	}
	if final == t0 {
		t.Fatal("the cold load finished in one preparation, so the fixture allocation proves nothing")
	}
	if loading := f.owner.FundingLoading(); len(loading) != 0 {
		t.Fatalf("funding inputs still loading after the cold load: %v", loading)
	}
	for _, coin := range rdCoins {
		f.requireTruth(last, coin)
	}
	if st := f.ledger.status(); st.Totals.Refused != 0 || st.Weight.Totals.Refused != 0 {
		t.Fatalf("the cold load refused %d requests and %d weight reads (%v)", st.Totals.Refused, st.Weight.Totals.Refused, st.Weight.Totals.RefusedByReason)
	}
	t.Logf("cold load completed between +%ds and +%ds", first-t0, final-t0)
}

func TestFeedFundingCheckpointSurvivesInterruption(t *testing.T) {
	t0 := rdBase()
	type step struct {
		name    string
		inject  func(f *fcFixture, coin string)
		clear   func(f *fcFixture, coin string)
		seal    func(f *fcFixture, d int64)
		restart bool
	}
	plain := func(f *fcFixture, d int64) { f.seal(d) }
	steps := []step{
		{name: "WeightRefusal", seal: plain,
			inject: func(f *fcFixture, coin string) {
				var once sync.Once
				hlInfoBeforeSendHook = func(c context.Context, reqType string) {
					if in, _ := c.Value(feedInputCtxKey{}).(string); in == "funding:"+coin && reqType == "fundingHistory" {
						once.Do(func() {
							zero := 0
							f.ledger.setLimits(&feedBudgetConfig{PerMinute: 60, Startup: 60, WeightPerMinute: &zero, WeightStartup: &zero, WeightReserve: &zero})
						})
					}
				}
			},
			clear: func(f *fcFixture, coin string) {
				hlInfoBeforeSendHook = nil
				w, ws, wr := 900, 100, 200
				f.ledger.setLimits(&feedBudgetConfig{PerMinute: 60, Startup: 60, WeightPerMinute: &w, WeightStartup: &ws, WeightReserve: &wr})
			}},
		{name: "RateLimitPause", seal: plain,
			inject: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { v.status["fundingHistory:"+coin] = http.StatusTooManyRequests })
			},
			clear: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { v.status = map[string]int{} })
			}},
		{name: "Timeout",
			inject: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) {
					v.delay = func(r rdRequest) time.Duration {
						if r.Type == "fundingHistory" && r.Coin == coin {
							return 900 * time.Millisecond
						}
						return 0
					}
				})
			},
			seal: func(f *fcFixture, d int64) {
				ctx, cancel := context.WithTimeout(f.ctx, 1200*time.Millisecond)
				defer cancel()
				prev := f.ctx
				f.ctx = ctx
				f.seal(d)
				f.ctx = prev
			},
			clear: func(f *fcFixture, coin string) { f.venue.set(func(v *rdVenue) { v.delay = nil }) }},
		{name: "ServerError", seal: plain,
			inject: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { v.fundingFail[coin] = true })
			},
			clear: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { delete(v.fundingFail, coin) })
			}},
		{name: "NoProgress", seal: plain,
			inject: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { v.fundingLag = 2000 * time.Hour })
			},
			clear: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { v.fundingLag = 0 })
			}},
		{name: "ConflictRestartsTheRange", seal: plain, restart: true,
			inject: func(f *fcFixture, coin string) {
				at := f.checkpoint(coin).boundary().TimeMs
				f.venue.set(func(v *rdVenue) { v.fundingConflict[coin] = at })
			},
			clear: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { delete(v.fundingConflict, coin) })
			}},
		{name: "MissingAnchorRestartsTheRange", seal: plain, restart: true,
			inject: func(f *fcFixture, coin string) {
				at := f.checkpoint(coin).boundary().TimeMs
				f.venue.set(func(v *rdVenue) { v.fundingDrop[coin] = at })
			},
			clear: func(f *fcFixture, coin string) {
				f.venue.set(func(v *rdVenue) { delete(v.fundingDrop, coin) })
			}},
		{name: "WiderFrameRestartsTheRange", seal: plain, restart: true,
			inject: func(f *fcFixture, coin string) { f.widen("1h", fcDeepRows+400); f.reinstall() },
			clear:  func(f *fcFixture, coin string) {}},
		{name: "UnrelatedReloadKeepsTheCheckpoint", seal: plain,
			inject: func(f *fcFixture, coin string) {
				f.consumers[0].Req.Keys[feedKeyFor("BTC", "5m")] += 10
				f.reinstall()
			},
			clear: func(f *fcFixture, coin string) {}},
		{name: "ProcessRestartStartsAFreshLoad", seal: plain, restart: true,
			inject: func(f *fcFixture, coin string) {
				owner := newMarketFeedOwner(f.clock.Now, f.logf)
				f.owner, f.sealer.owner = owner, owner
				src := newFeedRESTSource(owner, f.logf)
				src.backoff = time.Millisecond
				f.sealer.prepareFn = src.prepare
				f.reinstall()
			},
			clear: func(f *fcFixture, coin string) {}},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			f := newFCDeepFixture(t, time.Unix(t0-2000, 0).UTC())
			t.Cleanup(func() { hlInfoBeforeSendHook = nil })
			d := t0
			f.seal(d)
			coin := ""
			for _, c := range rdCoins {
				if run := f.checkpoint(c); run != nil && len(run.Records) > 0 {
					coin = c
				}
			}
			if coin == "" {
				t.Fatalf("no coin holds a checkpoint after the first preparation; logs %v", f.logsContaining("[feed-funding]"))
			}
			before := *f.checkpoint(coin)
			st.inject(f, coin)
			mark := f.venue.mark()
			d += 75
			st.seal(f, d)
			interrupted := fundingReads(f.venue.since(mark, ""), coin)
			after := f.checkpoint(coin)
			if !st.restart && st.name != "UnrelatedReloadKeepsTheCheckpoint" {
				if after == nil || after.StartMs != before.StartMs || len(after.Records) < len(before.Records) {
					t.Fatalf("checkpoint after the interruption = %+v, want the %d validated records from %d kept", after, len(before.Records), before.StartMs)
				}
			}
			st.clear(f, coin)
			if st.name == "RateLimitPause" {
				f.clock.Advance(2 * time.Minute)
				d += 150
			}
			resumeFrom := before.boundary().TimeMs
			if after != nil && after.StartMs == before.StartMs && len(after.Records) > 0 {
				resumeFrom = after.boundary().TimeMs
			}
			mark = f.venue.mark()
			doc := (*feedSealDoc)(nil)
			for i := 0; i < 30; i++ {
				d += 75
				doc, _ = f.seal(d)
				if f.fundingOf(doc, coin).HasRecords && f.checkpoint(coin) == nil {
					break
				}
			}
			f.requireTruth(doc, coin)
			reads := append(interrupted, fundingReads(f.venue.since(mark, ""), coin)...)
			rereadStart := false
			for _, r := range reads {
				if r.Start <= before.StartMs+2*hlFundingRecordStepMs && r.End-r.Start > 8*86_400_000 {
					rereadStart = true
				}
			}
			if rereadStart != st.restart {
				t.Fatalf("range restarted = %t, want %t; reads %+v", rereadStart, st.restart, reads)
			}
			if !st.restart {
				resumed := fundingReads(f.venue.since(mark, ""), coin)
				if len(resumed) > 0 && resumed[0].Start != resumeFrom {
					found := false
					for _, r := range resumed {
						found = found || r.Start == resumeFrom
					}
					if !found && st.name != "UnrelatedReloadKeepsTheCheckpoint" {
						t.Fatalf("resumed reads = %+v, want a read that rechecks the saved boundary %d", resumed, resumeFrom)
					}
				}
			}
			if st.name == "WiderFrameRestartsTheRange" && f.fundingOf(doc, coin).RecordsFromMs >= before.StartMs {
				t.Fatalf("coverage from %d after a wider frame, want earlier than %d", f.fundingOf(doc, coin).RecordsFromMs, before.StartMs)
			}
		})
	}
	t.Run("RemovalDropsTheCheckpointAndRecords", func(t *testing.T) {
		f := newFCDeepFixture(t, time.Unix(t0-2000, 0).UTC())
		f.seal(t0)
		req := &f.consumers[1].Req
		for id, e := range req.Strategies {
			if e.Coin == "SOL" && e.FundingRecords {
				delete(req.Strategies, id)
			}
		}
		req.Funding["SOL"] = feedFundingNeed{Scalar: true}
		f.reinstall()
		if run := f.checkpoint("SOL"); run != nil {
			t.Fatalf("a removed record reader left checkpoint %+v", run)
		}
		doc, _ := f.seal(t0 + 75)
		if e := f.fundingOf(doc, "SOL"); e.RecordsContract != 0 || e.HasRecords || len(e.Records) != 0 {
			t.Fatalf("a scalar-only coin sealed %+v, want no record coverage", e)
		}
	})
	t.Run("RecordsRecoverWithoutTheScalar", func(t *testing.T) {
		f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
		f.enforce(100, 100, 900, 100, 200)
		d := t0
		for ; d < t0+600; d += 75 {
			f.seal(d)
		}
		f.venue.set(func(v *rdVenue) { v.ctxBroken["ETH"] = true })
		var doc *feedSealDoc
		mark := f.venue.mark()
		for ; d < t0+1500; d += 75 {
			doc, _ = f.seal(d)
			f.requireTruth(doc, "ETH")
		}
		e := f.fundingOf(doc, "ETH")
		if e.HasScalar || !strings.Contains(e.ScalarError, "ETH") {
			t.Fatalf("ETH scalar = %t (%q), want it held with its own reason", e.HasScalar, e.ScalarError)
		}
		if btc := f.fundingOf(doc, "BTC"); !btc.HasScalar || btc.ScalarError != "" || btc.Current != rdScalar("BTC") {
			t.Fatalf("BTC scalar = %+v, want it independent of the ETH context", btc)
		}
		for _, r := range fundingReads(f.venue.since(mark, ""), "ETH") {
			if r.End-r.Start > 8*86_400_000 {
				t.Fatalf("a scalar failure made ETH reread %d..%d", r.Start, r.End)
			}
		}
		fc := f.context(doc, 1)
		if hold := fc.holdFor(f.byID["paper-ETH-1h-skew"]); hold.Held {
			t.Fatalf("the record reader is held by a scalar failure: %s", hold.Reason)
		}
		if hold := fc.holdFor(f.byID["paper-ETH-30m-neutral"]); !hold.Held {
			t.Fatal("the scalar reader evaluates without a current scalar")
		}
	})
}

func TestFeedFundingScalarSharing(t *testing.T) {
	t0 := rdBase()
	f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer {
		consumers := fcWorkload(t)
		req := &consumers[1].Req
		for id, e := range req.Strategies {
			if e.Coin == "SOL" && e.FundingRecords && e.Signal.Timeframe == "1h" {
				delete(req.Strategies, id)
			}
		}
		req.Funding["SOL"] = feedFundingNeed{Scalar: true, Records: true}.withReader(feedKeyFor("SOL", "30m"), hlBatchPythonDefaultOhlcvLimit)
		return consumers
	})
	f.enforce(200, 100, 900, 100, 200)
	modes := map[string]map[string]int{}
	for d := t0; d <= t0+1500; d += 75 {
		mark := f.venue.mark()
		logMark := len(f.logsContaining("mode=scalar"))
		doc, _ := f.seal(d)
		if n := len(f.venue.since(mark, "metaAndAssetCtxs")); n > 1 {
			t.Fatalf("deadline %d sent %d scalar context requests, want at most one", d, n)
		}
		for _, line := range f.logsContaining("mode=scalar")[logMark:] {
			for _, coin := range rdCoins {
				if !strings.Contains(line, "coin="+coin+" ") {
					continue
				}
				mode := "direct"
				if strings.Contains(line, "average=retained") {
					mode = "retained"
				}
				if modes[coin] == nil {
					modes[coin] = map[string]int{}
				}
				modes[coin][mode]++
				var from, to int64
				fmt.Sscanf(line[strings.Index(line, "range=["):], "range=[%d,%d]", &from, &to)
				f.owner.feedMu.Lock()
				got := f.owner.funding[coin].Avg7d
				f.owner.feedMu.Unlock()
				direct, err := fetchHyperliquidFundingHistory(context.Background(), coin, from, to)
				if err != nil {
					t.Fatal(err)
				}
				if want := hlFundingMean(direct); got != want || to-from != 7*86_400_000 {
					t.Fatalf("%s %s seven-day mean = %v over [%d, %d], the direct read gives %v", coin, mode, got, from, to, want)
				}
			}
		}
		if d > t0+300 {
			for _, coin := range rdCoins {
				if e := f.fundingOf(doc, coin); !e.HasScalar || e.Current != rdScalar(coin) {
					t.Fatalf("deadline %d %s scalar = %+v", d, coin, e)
				}
			}
		}
	}
	if modes["BTC"]["retained"] == 0 || modes["SOL"]["retained"] != 0 || modes["SOL"]["direct"] == 0 {
		t.Fatalf("scalar averages = %v, want BTC from its proved 201-hour range and SOL direct because its 100-hour range does not cover seven days", modes)
	}
	entry := f.owner.fundingEntry("BTC")
	if _, proved := entry.provedMean(entry.RecordsFromMs-1, entry.RecordsToMs); proved {
		t.Fatal("a window that starts before the proved range is accepted")
	}
	if _, proved := entry.provedMean(entry.RecordsFromMs, entry.RecordsThroughMs+1); proved {
		t.Fatal("a window that ends after the proved range is accepted")
	}
}

func TestFeedRESTPreparationTiming(t *testing.T) {
	real := time.Now().UTC()
	hour := real.Truncate(time.Hour)
	if left := hour.Add(time.Hour).Sub(real); left < 40*time.Second {
		time.Sleep(left + time.Second)
		real = time.Now().UTC()
	}
	f := newFCFixture(t, real.Add(-1200*time.Second), func() []feedConsumer { return fcWorkload(t) })
	f.enforce(300, 100, 900, 100, 200)
	deadline := real.Unix() / 300 * 300
	f.sealer.prepare = time.Hour
	for d := deadline - 900; d < deadline; d += 75 {
		f.seal(d)
	}
	f.sealer.prepare = feedSealPrepareBudget
	stop := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				f.clock.mu.Lock()
				if now := time.Now().UTC(); now.After(f.clock.now) {
					f.clock.now = now
				}
				f.clock.mu.Unlock()
			}
		}
	}()
	defer func() { close(stop); ticker.Wait() }()
	f.venue.set(func(v *rdVenue) { v.delay = func(rdRequest) time.Duration { return 800 * time.Millisecond } })
	mark := f.venue.mark()
	started := time.Now()
	f.sealer.settle = time.Since(time.Unix(deadline, 0))
	f.sealer.sealOne(f.ctx, deadline)
	took := time.Since(started)
	f.sealer.mu.Lock()
	seal := f.sealer.ring[deadline]
	f.sealer.mu.Unlock()
	if seal == nil || took >= feedSealPrepareBudget {
		t.Fatalf("preparation with 800ms reads took %s (seal %v), want a seal inside %s", took, seal != nil, feedSealPrepareBudget)
	}
	doc, err := decodeFeedSeal(seal.Bytes, deadline)
	if err != nil {
		t.Fatal(err)
	}
	f.requireUsable(doc)
	reqs := f.venue.since(mark, "")
	midsAt, fundingAt := time.Time{}, time.Time{}
	for _, r := range reqs {
		if r.Type == "allMids" {
			midsAt = r.At
		}
		if fundingAt.IsZero() && (r.Type == "fundingHistory" || r.Type == "metaAndAssetCtxs") {
			fundingAt = r.At
		}
	}
	if midsAt.IsZero() || (!fundingAt.IsZero() && !fundingAt.After(midsAt)) {
		t.Fatalf("mids arrived at %s and funding at %s, want mids first", midsAt, fundingAt)
	}
	if took+feedBudgetWindow > 75*time.Second {
		t.Fatalf("the preparation settled its last read %s after it started, so its charges overlap the next 75s preparation", took)
	}
	t.Logf("800ms reads: %d requests in %s", len(reqs), took.Round(time.Millisecond))
}

func TestFeedRESTAdmissionOrderIsFixed(t *testing.T) {
	t0 := rdBase()
	f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
	f.enforce(200, 100, 900, 100, 200)
	for d := t0; d < t0+600; d += 75 {
		f.seal(d)
	}
	candle, _ := hlInfoWeight("candleSnapshot", 3)
	mids, _ := hlInfoWeight("allMids", 0)
	order := restRefreshOrder(fullCycleRequirements(f.union), f.union)
	const admitted = 11
	for round := 0; round < 4; round++ {
		rng := rand.New(rand.NewSource(int64(round)))
		var mu sync.Mutex
		f.venue.set(func(v *rdVenue) {
			v.delay = func(rdRequest) time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return time.Duration(rng.Intn(12)) * time.Millisecond
			}
		})
		f.enforce(200, 100, admitted*candle+mids, 0, 200)
		doc, reqs := f.seal(t0 + 600 + int64(round)*75)
		sent := rdCandleKeys(reqs)
		if len(sent) != admitted {
			t.Fatalf("round %d sent %d candle reads, want the %d that fit", round, len(sent), admitted)
		}
		for i, cr := range order {
			got := sent[cr.Key.PayloadID()] == 1
			if got != (i < admitted) {
				t.Fatalf("round %d read %s = %t at position %d, want the first %d keys of the refresh order", round, cr.Key.PayloadID(), got, i, admitted)
			}
			if r := rdSealKey(t, doc, cr.Key).Readiness; r.Ready != (i < admitted) {
				t.Fatalf("round %d sealed %s ready=%t, want it to match its admission", round, cr.Key.PayloadID(), r.Ready)
			}
		}
		if len(doc.Mids) != len(rdCoins) {
			t.Fatalf("round %d sealed %d mids, want the kept credit to serve them", round, len(doc.Mids))
		}
	}
	t.Run("PauseWithWorkInFlight", func(t *testing.T) {
		f.enforce(200, 100, 900, 100, 200)
		limited := order[2].Key
		release := make(chan struct{})
		f.venue.set(func(v *rdVenue) {
			v.delay = func(r rdRequest) time.Duration {
				if r.Type == "candleSnapshot" && r.Coin == limited.Symbol && r.Interval == limited.Timeframe {
					return 30 * time.Millisecond
				}
				select {
				case <-release:
				case <-time.After(150 * time.Millisecond):
				}
				return 0
			}
			v.status["candleSnapshot:"+limited.Symbol] = 0
		})
		f.venue.set(func(v *rdVenue) { v.limitKey = limited })
		mark := f.venue.mark()
		d := t0 + 600 + 4*75 + 300
		doc, _ := f.seal(d)
		close(release)
		reqs := f.venue.since(mark, "")
		if n := len(reqs); n > feedRESTCandleWorkers+1 {
			t.Fatalf("%d requests reached the venue, want only the %d that were in flight when the pause opened", n, feedRESTCandleWorkers)
		}
		for _, r := range reqs {
			if r.Type != "candleSnapshot" {
				t.Fatalf("a %s read was sent after the pause", r.Type)
			}
		}
		st := f.ledger.status()
		if st.Weight.Pending != 0 || st.Weight.InFlight != 0 || st.Totals.RateLimited != 1 {
			t.Fatalf("after the pause pending=%d in_flight=%d rate_limited=%d, want every permit closed and one 429", st.Weight.Pending, st.Weight.InFlight, st.Totals.RateLimited)
		}
		if r := rdSealKey(t, doc, limited).Readiness; r.Ready || r.Status != string(feedStatusRateLimited) {
			t.Fatalf("limited key = %+v", r)
		}
		if r := rdSealKey(t, doc, order[24].Key).Readiness; r.Ready {
			t.Fatalf("a key behind the pause = %+v, want not ready", r)
		}
	})
}

func TestFeedFundingCoverageFollowsItsReaders(t *testing.T) {
	t0 := rdBase()
	build := func(withLag bool) []feedConsumer {
		live, paper := fcStrategies()
		var kept []StrategyConfig
		for _, sc := range paper {
			if withLag || !strings.HasSuffix(sc.ID, "-4h-lag") {
				kept = append(kept, sc)
			}
		}
		return []feedConsumer{rdConsumer(t, "/live.json", live), rdConsumer(t, "/paper.json", kept)}
	}
	seal := func(t *testing.T, f *fcFixture, d int64) (*feedSealDoc, map[string]int64) {
		t.Helper()
		starts := map[string]int64{}
		var doc *feedSealDoc
		for i := int64(0); i < 8; i++ {
			mark := f.venue.mark()
			doc, _ = f.seal(d + i*75)
			for _, coin := range rdCoins {
				for _, r := range fundingReads(f.venue.since(mark, ""), coin) {
					if e := f.fundingOf(doc, coin); e.HasRecords && r.End-r.Start > 8*86_400_000 && starts[coin] == 0 {
						starts[coin] = r.Start
					}
				}
			}
		}
		return doc, starts
	}
	without := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return build(false) })
	docWithout, startsWithout := seal(t, without, t0)
	with := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return build(true) })
	docWith, startsWith := seal(t, with, t0)
	if len(with.union.Order) != 25 || len(without.union.Order) != 20 {
		t.Fatalf("fixtures hold %d and %d keys, want 25 and 20", len(with.union.Order), len(without.union.Order))
	}
	for _, coin := range rdCoins {
		if startsWith[coin] == 0 || startsWith[coin] != startsWithout[coin] {
			t.Fatalf("%s funding query starts at %d with the 4h lead-lag keys and %d without them", coin, startsWith[coin], startsWithout[coin])
		}
		a, b := with.fundingOf(docWith, coin), without.fundingOf(docWithout, coin)
		if a.RecordsFromMs != b.RecordsFromMs || len(a.Records) != len(b.Records) || a.RecordsFromMs < (t0-210*3600)*1000 {
			t.Fatalf("%s coverage from %d (%d records) with and from %d (%d) without the 4h keys, want the 1h reader range", coin, a.RecordsFromMs, len(a.Records), b.RecordsFromMs, len(b.Records))
		}
	}

	check := func(t *testing.T, f *fcFixture, doc *feedSealDoc) {
		t.Helper()
		fc := f.context(doc, 1)
		for id, entry := range fc.Requirements.Strategies {
			if !entry.FundingRecords {
				continue
			}
			raw, err := fc.singleCheckPayload(f.byID[id])
			if err != nil {
				t.Fatalf("%s payload: %v", id, err)
			}
			var env marketStdinEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			frame := env.Market.Frames[entry.Signal.PayloadID()]
			rows := frame.Rows
			endMs := env.Market.SealedAtMs
			if entry.ClosedBar {
				intervalMs, _ := hlCandleIntervalMs(entry.Signal.Timeframe)
				boundary := env.Market.DecisionCutoffMs / intervalMs * intervalMs
				closed := rows[:0:0]
				for i, b := range frame.Timing.Bars {
					if int64(b[0].(float64))+intervalMs <= env.Market.DecisionCutoffMs {
						closed = append(closed, rows[i])
					}
				}
				rows, endMs = closed, boundary
			}
			if limit := hlBatchPythonDefaultOhlcvLimit; len(rows) > limit {
				rows = rows[len(rows)-limit:]
			}
			if len(rows) != hlBatchPythonDefaultOhlcvLimit {
				t.Fatalf("%s selects %d rows, want the full check frame", id, len(rows))
			}
			start := rows[0].TimestampMs
			got := env.Market.Funding[entry.Coin]
			if got.RecordsContract != feedFundingRecordsContract || got.RecordsFromMs > start {
				t.Fatalf("%s coverage %+v does not reach its first selected row %d", id, got.RecordsFromMs, start)
			}
			var used []feedFundingRecord
			for _, r := range got.Records {
				if r.TimeMs >= start && r.TimeMs <= endMs {
					used = append(used, r)
				}
			}
			want := f.truth(entry.Coin, start, endMs)
			if len(used) != len(want) || len(want) == 0 {
				t.Fatalf("%s uses %d funding records from %d, the full-range reference has %d", id, len(used), start, len(want))
			}
			for i := range want {
				if used[i] != want[i] {
					t.Fatalf("%s record %d = %+v, reference %+v", id, i, used[i], want[i])
				}
			}
		}
	}
	check(t, with, docWith)
	t.Run("HourBoundary", func(t *testing.T) {
		doc, _ := with.seal(t0 + 3600)
		check(t, with, doc)
	})
	t.Run("RowsWithoutANativeClose", func(t *testing.T) {
		f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return nil })
		f.consumers = build(true)
		f.venue.set(func(v *rdVenue) { v.noClose = true })
		f.reinstall()
		doc, _ := seal(t, f, t0)
		key := rdSealKey(t, doc, feedKeyFor("BTC", "1h"))
		if key.Bars[0].HasClose {
			t.Fatal("the fixture sealed native closes")
		}
		check(t, f, doc)
	})
	t.Run("DuplicateStrategyIDsAcrossConsumers", func(t *testing.T) {
		mk := func(path, coin, tf string) feedConsumer {
			return rdConsumer(t, path, []StrategyConfig{rdStrategy("same-id", "funding_skew", coin, tf, 300, false, false, false)})
		}
		union := unionFeedRequirements([]feedConsumer{mk("/a.json", "BTC", "30m"), mk("/b.json", "BTC", "1h"), mk("/c.json", "ETH", "30m")})
		if got := union.Funding["BTC"].readerList(); got != "BTC|1h:200,BTC|30m:200" || union.Funding["ETH"].readerList() != "ETH|30m:200" {
			t.Fatalf("union readers BTC=%s ETH=%s, want every consumer's reader kept apart from its strategy id", got, union.Funding["ETH"].readerList())
		}
	})
	t.Run("UnreadableConsumerKeepsItsReaders", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "paper.json")
		write := func(body string) {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write(fmt.Sprintf(`{"config_version": %d, "market_feed": "shared", "db_file": %q, "log_dir": %q,
			"shared_market_feed": {"primary_socket": "/run/fc/feed.sock"},
			"strategies": [{"id": "skew", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": ["funding_skew", "BTC", "1h", "--mode=paper"], "capital": 1000, "max_drawdown_pct": 50}]}`, CurrentConfigVersion, filepath.Join(dir, "s.db"), dir))
		first := loadFeedConsumers([]string{path}, "/run/fc/feed.sock", nil, time.Now())
		if !first[0].Loaded {
			t.Fatalf("consumer did not load: %s", first[0].Err)
		}
		write("{")
		again := loadFeedConsumers([]string{path}, "/run/fc/feed.sock", map[string]feedConsumer{path: first[0]}, time.Now())
		a, b := unionFeedRequirements(first), unionFeedRequirements(again)
		if !again[0].Retained || a.Funding["BTC"].readerList() != b.Funding["BTC"].readerList() || fundingCoverageID(a.Funding) != fundingCoverageID(b.Funding) {
			t.Fatalf("retained=%t readers %s, want the last loaded contribution %s", again[0].Retained, b.Funding["BTC"].readerList(), a.Funding["BTC"].readerList())
		}
	})
	t.Run("ExpandedLookbackHoldsUntilItIsProved", func(t *testing.T) {
		f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return build(true) })
		before, _ := seal(t, f, t0)
		f.widen("1h", 900)
		fc := f.context(before, 1)
		wideHold := fc.holdFor(f.byID["paper-ETH-1h-skew"])
		if !wideHold.Held {
			t.Fatal("a retained seal of the old coverage serves the expanded lookback")
		}
		if hold := fc.holdFor(f.byID["paper-ETH-30m-skew"]); hold.Held {
			t.Fatalf("an unaffected reader is held: %s", hold.Reason)
		}
		f.reinstall()
		f.enforce(300, 100, 900, 100, 200)
		var doc *feedSealDoc
		for d := t0 + 1200; d < t0+1800; d += 75 {
			doc, _ = f.seal(d)
		}
		if hold := f.context(doc, 1).holdFor(f.byID["paper-ETH-1h-skew"]); hold.Held {
			t.Fatalf("the expanded reader is still held: %s", hold.Reason)
		}
		f.requireTruth(doc, "ETH")
	})
	t.Run("ReloadDuringPreparationSealsNoProof", func(t *testing.T) {
		f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return build(true) })
		seal(t, f, t0)
		for _, at := range []string{"sealer", "owner"} {
			d := t0 + 1200
			if at == "owner" {
				d += 75
			}
			inner := f.sealer.prepareFn
			f.sealer.prepareFn = func(ctx context.Context, key int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
				rep := inner(ctx, key, reqs, coverage)
				f.widen("1h", 400+int(d-t0))
				next := unionFeedRequirements(f.consumers)
				select {
				case <-f.owner.ApplyGeneration(f.ctx, next):
				case <-time.After(30 * time.Second):
					t.Error("generation did not publish")
				}
				if at == "sealer" {
					f.sealer.setGeneration(next, unionFeedCadences(f.consumers), f.clock.Now().Add(-time.Hour))
				}
				return rep
			}
			doc, _ := f.seal(d)
			f.sealer.prepareFn = inner
			for _, coin := range rdCoins {
				e := f.fundingOf(doc, coin)
				if e.HasRecords || e.RecordsError != feedFundingCoverageChanged {
					t.Fatalf("%s: a reload at the %s sealed %s coverage (%q)", at, at, coin, e.RecordsError)
				}
			}
			if hold := f.context(doc, 1).holdFor(f.byID["paper-BTC-30m-skew"]); !hold.Held {
				t.Fatalf("%s: an unproved range evaluates", at)
			}
			if r := rdSealKey(t, doc, feedKeyFor("BTC", "5m")).Readiness; !r.Ready {
				t.Fatalf("%s: a reload blocked the candles: %+v", at, r)
			}
			f.sealer.setGeneration(unionFeedRequirements(f.consumers), unionFeedCadences(f.consumers), f.clock.Now().Add(-time.Hour))
		}
		var doc *feedSealDoc
		for d := t0 + 1500; d < t0+2400; d += 75 {
			doc, _ = f.seal(d)
		}
		f.requireTruth(doc, "BTC")
	})
}

func TestFeedFundingProofOnTheWire(t *testing.T) {
	t0 := rdBase()
	f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
	var doc *feedSealDoc
	for d := t0; d < t0+600; d += 75 {
		doc, _ = f.seal(d)
	}
	if doc.V != feedSealVersionFunding || f.sealer.coverageSealVersion() != feedSealVersionFunding {
		t.Fatalf("seal v%d, advertised v%d, want v%d for record coverage", doc.V, f.sealer.coverageSealVersion(), feedSealVersionFunding)
	}
	encode := func(mutate func(e *feedSealFunding)) error {
		cp := *doc
		cp.Funding = append([]feedSealFunding(nil), doc.Funding...)
		mutate(&cp.Funding[0])
		cp.V = cp.contentVersion()
		blob, _, err := encodeFeedSeal(&cp)
		if err != nil {
			t.Fatal(err)
		}
		_, err = decodeFeedSeal(blob, cp.Key)
		return err
	}
	if err := encode(func(*feedSealFunding) {}); err != nil {
		t.Fatalf("an unchanged seal is refused: %v", err)
	}
	for name, mutate := range map[string]func(e *feedSealFunding){
		"unknown contract":      func(e *feedSealFunding) { e.RecordsContract = 2 },
		"from after to":         func(e *feedSealFunding) { e.RecordsFromMs = e.RecordsToMs + 1 },
		"overclaimed through":   func(e *feedSealFunding) { e.RecordsThroughMs += hlFundingRecordStepMs },
		"record before from":    func(e *feedSealFunding) { e.RecordsFromMs = e.Records[0].TimeMs + 1 },
		"coverage with no data": func(e *feedSealFunding) { e.HasRecords, e.Records = false, []feedFundingRecord{} },
		"no reason": func(e *feedSealFunding) {
			*e = feedSealFunding{Coin: e.Coin, Source: e.Source, RecordsContract: 1, Records: []feedFundingRecord{}}
		},
		"fields, no contract": func(e *feedSealFunding) { e.RecordsContract = 0 },
	} {
		if err := encode(mutate); err == nil {
			t.Fatalf("%s: a malformed proof decodes", name)
		}
	}

	dir, err := os.MkdirTemp("", "fcw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: filepath.Join(dir, "none.sock")}})
	for _, tc := range []struct {
		version    int
		compatible bool
	}{{feedSealVersionFunding, true}, {feedSealVersionClosedReuse, true}, {feedSealVersionFunding + 1, false}} {
		_, _, err := client.roundTrip(context.Background(), oiTestOldPeer(t, dir, tc.version), feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe})
		if (err == nil) != tc.compatible {
			t.Fatalf("seal v%d peer: %v, want compatible=%t", tc.version, err, tc.compatible)
		}
	}
	d := f.sealer.describe()
	endpoint := func(mutate func(d *feedDescribe)) []string {
		cp := *d
		cp.Funding = append([]feedDescribeFunding(nil), d.Funding...)
		mutate(&cp)
		_, problems := sharedFeedCompatibilityLines("test", []feedEndpointDescription{{Endpoint: sharedFeedEndpoint{Name: "primary"}, Describe: &cp}}, f.consumers[1].Req, f.consumers[1].Cadences)
		return problems
	}
	if problems := endpoint(func(*feedDescribe) {}); len(problems) != 0 {
		t.Fatalf("the feed does not cover its own consumer: %v", problems)
	}
	if problems := endpoint(func(d *feedDescribe) { d.Funding[0].RecordsContract, d.Funding[0].Readers = 0, nil }); len(problems) != 1 {
		t.Fatalf("a feed with no record contract reports %v, want one gap", problems)
	}
	if problems := endpoint(func(d *feedDescribe) {
		d.Funding[0].Readers = append([]feedDescribeFundingReader(nil), d.Funding[0].Readers...)
		d.Funding[0].Readers[0].Rows--
	}); len(problems) != 1 {
		t.Fatalf("a feed with a shorter reader reports %v, want one gap", problems)
	}

	entry := f.consumers[1].Req.Strategies["paper-ETH-1h-skew"]
	held := func(mutate func(s *marketSnapshot)) (bool, string) {
		snap := doc.snapshot()
		mutate(snap)
		fc := &marketFeedContext{Enabled: true, Requirements: f.consumers[1].Req, Snapshot: snap}
		if peer := fc.holdFor(f.byID["paper-BTC-30m-skew"]); peer.Held {
			t.Fatalf("an unaffected reader is held: %s", peer.Reason)
		}
		hold := fc.holdFor(f.byID["paper-ETH-1h-skew"])
		return hold.Held, hold.Reason
	}
	edit := func(fn func(e *feedFunding)) func(s *marketSnapshot) {
		return func(s *marketSnapshot) {
			e := s.funding["ETH"]
			fn(&e)
			s.funding["ETH"] = e
		}
	}
	if h, why := held(func(*marketSnapshot) {}); h {
		t.Fatalf("a proved reader is held: %s", why)
	}
	for name, mutate := range map[string]func(s *marketSnapshot){
		"pre-contract feed": edit(func(e *feedFunding) { e.RecordsContract = 0 }),
		"stale read":        edit(func(e *feedFunding) { e.RecordsToMs = doc.SealedAtMs - feedFundingTTL.Milliseconds() - 1 }),
		"open boundary":     edit(func(e *feedFunding) { e.RecordsThroughMs = t0*1000 - 1 }),
		"filled candle frame that starts earlier": func(s *marketSnapshot) {
			backup := rdSealKey(t, doc, entry.Signal)
			bars := append([]feedSealBar(nil), backup.Bars...)
			first := bars[0]
			for i := 0; i < 5; i++ {
				first.OpenMs, first.CloseMs = first.OpenMs-backup.IntervalMs, first.CloseMs-backup.IntervalMs
				bars = append([]feedSealBar{first}, bars...)
			}
			bars = bars[:len(bars)-3]
			backup.Bars, backup.Readiness.Bars = bars, len(bars)
			fill := &feedSealDoc{Keys: []feedSealKey{backup}}
			cp := *s.keys[entry.Signal]
			cp.Readiness.Ready = false
			s.keys[entry.Signal] = &cp
			if got := applyKeyFills(s, fill, "backup", []marketFeedKey{entry.Signal}); len(got) != 1 {
				t.Fatalf("fill = %v", got)
			}
			s.keys[entry.Signal].Readiness.Required = entry.SignalLookback
		},
	} {
		if h, why := held(mutate); !h {
			t.Fatalf("%s: the affected reader evaluates", name)
		} else {
			t.Logf("%s: %s", name, why)
		}
	}
	snap := doc.snapshot()
	edit(func(e *feedFunding) { e.RecordsContract = 0 })(snap)
	gaps := applySealCoverage(snap, cycleRequirementsForDue([]StrategyConfig{f.byID["paper-ETH-1h-skew"]}, f.consumers[1].Req))
	if len(gaps) == 0 || !strings.Contains(strings.Join(gaps, ";"), "coverage contract 0") {
		t.Fatalf("seal coverage gaps = %v, want the missing contract named", gaps)
	}
}

func TestFeedWeightRecoveryNeedsEveryRefusedInput(t *testing.T) {
	hour := rdBase()
	cases := []struct {
		name   string
		input  string
		target func(input, reqType string) bool
		spoil  func(v *rdVenue, on bool)
		before func(f *fcFixture)
		remove func(f *fcFixture)
	}{
		{name: "CandleHistory", input: "SOL|4h", target: func(input, reqType string) bool { return input == "SOL|4h" },
			spoil: func(v *rdVenue, on bool) { v.badRows["SOL|4h"] = on },
			before: func(f *fcFixture) {
				f.consumers[1].Req.Keys[feedKeyFor("SOL", "4h")] += 40
				f.reinstall()
			}},
		{name: "Candles", input: "SOL|4h", target: func(input, reqType string) bool { return input == "SOL|4h" },
			spoil: func(v *rdVenue, on bool) { v.fail["SOL|4h"] = on },
			remove: func(f *fcFixture) {
				req := &f.consumers[1].Req
				delete(req.Strategies, "paper-SOL-4h-lag")
				delete(req.Keys, feedKeyFor("SOL", "4h"))
				req.finalize()
			}},
		{name: "Mids", input: "mids", target: func(input, reqType string) bool { return reqType == "allMids" },
			spoil: func(v *rdVenue, on bool) { v.midsMissing["BTC"] = on }},
		{name: "StrategyFunding", input: "funding:ETH", target: func(input, reqType string) bool { return input == "funding:ETH" },
			spoil: func(v *rdVenue, on bool) { v.fundingFail["ETH"] = on },
			remove: func(f *fcFixture) {
				req := &f.consumers[1].Req
				for id, e := range req.Strategies {
					if e.Coin == "ETH" && (e.FundingRecords || e.FundingScalar) {
						delete(req.Strategies, id)
					}
				}
				delete(req.Funding, "ETH")
			}},
		{name: "AccountingFunding", input: "accounting_funding:HYPE", target: func(input, reqType string) bool { return input == "accounting_funding:HYPE" },
			spoil: func(v *rdVenue, on bool) { v.fundingFail["HYPE"] = on },
			remove: func(f *fcFixture) {
				for i := range f.consumers {
					req := &f.consumers[i].Req
					var kept []string
					for _, c := range req.AccountingCoins {
						if c != "HYPE" {
							kept = append(kept, c)
						}
					}
					req.AccountingCoins = kept
				}
			}},
	}
	for _, tc := range cases {
		for _, removed := range []bool{false, true} {
			if removed && tc.remove == nil {
				continue
			}
			name := tc.name
			if removed {
				name += "Removed"
			}
			t.Run(name, func(t *testing.T) {
				f := newFCFixture(t, time.Unix(hour-4000, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
				t.Cleanup(func() { hlInfoBeforeSendHook = nil })
				f.enforce(300, 100, 900, 100, 200)
				var alerts []string
				f.sealer.rateLimitSend = func(msg string) { alerts = append(alerts, msg) }
				f.sealer.alertHook = func(string) {}
				counted := func(kind string) (n int) {
					for _, a := range alerts {
						if strings.HasPrefix(a, "**MARKET FEED WEIGHT "+kind+"**") {
							n++
						}
					}
					return n
				}
				d := hour - 1800
				for ; d < hour; d += 75 {
					f.seal(d)
				}
				if len(alerts) != 0 {
					t.Fatalf("warm-up alerts = %q", alerts)
				}
				limit := func(w int) {
					ws, wr := 100, 200
					f.ledger.setLimits(&feedBudgetConfig{PerMinute: 300, Startup: 100, WeightPerMinute: &w, WeightStartup: &ws, WeightReserve: &wr})
				}
				var refuse sync.Once
				fired := false
				hlInfoBeforeSendHook = func(c context.Context, reqType string) {
					if in, _ := c.Value(feedInputCtxKey{}).(string); tc.target(in, reqType) {
						refuse.Do(func() { fired = true; limit(0) })
					}
				}
				for ; !fired && d < hour+900; d += 75 {
					f.seal(d)
				}
				hlInfoBeforeSendHook = nil
				limit(900)
				if !fired || counted("REFUSED") != 1 || !strings.Contains(strings.Join(alerts, "\n"), tc.input) {
					t.Fatalf("fired=%t alerts=%q, want one refusal that names %s", fired, alerts, tc.input)
				}
				if !strings.Contains(alerts[0], "Consumers can hold entries when a required input is unavailable from the selected feed") {
					t.Fatalf("alert = %q", alerts[0])
				}
				f.venue.set(func(v *rdVenue) { tc.spoil(v, true) })
				if tc.before != nil {
					tc.before(f)
				}
				for end := d + 600; d < end; d += 75 {
					f.seal(d)
					if counted("RECOVERED") != 0 {
						t.Fatalf("a recovery alert was sent while %s is unusable: %q", tc.input, alerts)
					}
				}
				if outstanding := f.owner.weightRefusedInputs(); len(outstanding) != 1 || outstanding[0] != tc.input {
					t.Fatalf("outstanding = %v, want only %s: every other refused input recovered", outstanding, tc.input)
				}
				if removed {
					tc.remove(f)
					f.reinstall()
					for end := d + 300; d < end; d += 75 {
						f.seal(d)
					}
					for _, a := range alerts {
						if strings.Contains(a, "RECOVERED") && strings.Contains(a, tc.input) {
							t.Fatalf("removing %s reported its recovery: %q", tc.input, a)
						}
					}
					if st := f.ledger.status().Weight.Episode; st.Open {
						t.Fatalf("the episode is still open after the removal: %+v", st)
					}
					return
				}
				f.venue.set(func(v *rdVenue) { tc.spoil(v, false) })
				for end := d + 900; d < end; d += 75 {
					f.seal(d)
				}
				if counted("RECOVERED") != 1 || counted("REFUSED") != 1 {
					t.Fatalf("alerts = %q, want one refusal and one recovery", alerts)
				}
				if last := alerts[len(alerts)-1]; !strings.Contains(last, tc.input) {
					t.Fatalf("recovery alert %q does not name %s", last, tc.input)
				}
			})
		}
	}
	t.Run("OnlyRefusedInputRemoved", func(t *testing.T) {
		f := newFCFixture(t, time.Unix(hour-4000, 0).UTC(), func() []feedConsumer { return fcWorkload(t) })
		t.Cleanup(func() { hlInfoBeforeSendHook = nil })
		f.enforce(300, 100, 900, 100, 200)
		var alerts []string
		f.sealer.rateLimitSend = func(msg string) { alerts = append(alerts, msg) }
		d := hour - 1800
		for ; d < hour-900; d += 75 {
			f.seal(d)
		}
		f.ledger.countWeightRefusal(feedAcquireSpec{reason: string(feedRestFunding), reqType: "fundingHistory", input: "funding:SOL"}, feedWeightCauseExhausted)
		f.seal(d)
		req := &f.consumers[1].Req
		for id, e := range req.Strategies {
			if e.Coin == "SOL" && (e.FundingRecords || e.FundingScalar) {
				delete(req.Strategies, id)
			}
		}
		delete(req.Funding, "SOL")
		f.reinstall()
		f.seal(d + 75)
		if len(alerts) != 1 || !strings.Contains(alerts[0], "WEIGHT REFUSED") || f.ledger.status().Weight.Episode.Open {
			t.Fatalf("alerts = %q, episode open = %t; want the refusal only and a closed episode", alerts, f.ledger.status().Weight.Episode.Open)
		}
		if lines := f.logsContaining("status=" + feedWeightEventRetired); len(lines) != 1 {
			t.Fatalf("retirement log = %q, want one line", lines)
		}
	})
}

func TestFeedDrawdownCadenceAgreesWithTheFeed(t *testing.T) {
	cfg := &Config{IntervalSeconds: 300, PortfolioRisk: &PortfolioRiskConfig{WarnThresholdPct: 80}}
	cfg.Strategies = []StrategyConfig{
		rdStrategy("eligible", "rsi", "BTC", "5m", 300, false, false, true),
		rdStrategy("faster", "rsi", "BTC", "5m", 60, false, false, true),
		rdStrategy("ineligible", "rsi", "BTC", "5m", 300, false, false, false),
		rdStrategy("calm", "rsi", "BTC", "5m", 300, false, false, true),
	}
	states := map[string]*StrategyState{}
	for _, sc := range cfg.Strategies {
		st := &StrategyState{}
		st.RiskState.MaxDrawdownPct, st.RiskState.CurrentDrawdownPct = sc.MaxDrawdownPct, 19
		if sc.ID == "calm" {
			st.RiskState.CurrentDrawdownPct = 1
		}
		states[sc.ID] = st
	}
	intervals := effectiveStrategyIntervals(cfg.Strategies, states, cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(cfg))
	want := map[string]int{"eligible": 75, "faster": 60, "ineligible": 300, "calm": 300}
	for id, sec := range want {
		if intervals[id] != sec {
			t.Fatalf("intervals = %v, want %v", intervals, want)
		}
	}
	cfg.Strategies = append(cfg.Strategies[:1:1], cfg.Strategies[2:]...)
	cadences := feedConsumerCadences(cfg)
	if formatCadences(cadences) != "75s,300s" {
		t.Fatalf("feed cadences = %v, want 75s and 300s", cadences)
	}
	base := rdBase()
	sealer := newFeedSealer(newMarketFeedOwner(nil, nil), feedSourceREST, "i", func() time.Time { return time.Unix(base, 0) }, nil)
	sealer.setGeneration(feedRequirements{}, cadences, time.Unix(base-3600, 0))
	sched := &sharedFeedSchedule{Cadences: cadences, Window: newSharedFeedClientWithEndpoints(nil).giveUpWindow()}
	last := map[string]feedEvaluationMark{}
	delete(intervals, "faster")
	for now := base; now < base+900; now += 7 {
		at := now
		if now%900 > 400 && now%900 < 500 {
			continue
		}
		_, marks, _ := computeDueSet(time.Unix(at, 0), cfg, intervals, map[string]time.Time{}, last, true, sched)
		for id, m := range marks {
			key := m.Deadline.Unix()
			sealer.mu.Lock()
			served := sealer.onScheduleLocked(key)
			sealer.mu.Unlock()
			if !served {
				t.Fatalf("%s at +%ds selects deadline %d, which the feed does not schedule", id, at-base, key-base)
			}
			if at-key > int64(sched.Window/time.Second) {
				t.Fatalf("%s at +%ds selects a deadline %ds old", id, at-base, at-key)
			}
			last[id] = m
		}
	}
	if last["eligible"].Deadline.Unix()-base < 825 || last["ineligible"].Deadline.Unix()%300 != 0 {
		t.Fatalf("last evaluations = %v", last)
	}
}

func TestFeedFundingEmbeddedOwnerReadsDueReaders(t *testing.T) {
	t0 := rdBase()
	f := newFCFixture(t, time.Unix(t0-2000, 0).UTC(), func() []feedConsumer {
		consumers := fcWorkload(t)
		for i := range consumers {
			consumers[i].Req.AccountingCoins = nil
		}
		return consumers
	})
	f.ledger = newFeedRequestLedger(nil, false, f.clock.Now)
	f.ctx = withFeedLedger(context.Background(), f.ledger)
	req := unionFeedRequirements(f.consumers)
	req.Strategies = f.consumers[1].Req.Strategies
	cycle := func(d int64, ids ...string) (*marketSnapshot, cycleMarketRequirements, []rdRequest) {
		t.Helper()
		f.clock.mu.Lock()
		f.clock.now = time.Unix(d, 0).UTC()
		f.clock.mu.Unlock()
		var due []StrategyConfig
		for _, id := range ids {
			due = append(due, f.byID[id])
		}
		reqs := cycleRequirementsForDue(due, req)
		mark := f.venue.mark()
		snap := sealCycleMarketSnapshot(f.ctx, f.owner, reqs, feedSealEvaluationID(d), f.clock.Now())
		return snap, reqs, f.venue.since(mark, "")
	}
	usable := func(snap *marketSnapshot, id string) (bool, string) {
		fc := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap}
		hold := fc.holdFor(f.byID[id])
		return !hold.Held, hold.Reason
	}
	_, _, reqs := cycle(t0+60, "paper-ETH-1h-skew", "paper-BTC-30m-skew")
	if len(fundingReads(reqs, "ETH")) == 0 || len(fundingReads(reqs, "SOL")) != 0 {
		t.Fatalf("funding reads = %+v, want only the due coins", fundingReads(reqs, ""))
	}
	if e := f.owner.fundingEntry("BTC"); e.RecordsFromMs < (t0-120*3600)*1000 {
		t.Fatalf("BTC range starts at %d, want the due 30m reader's range only", e.RecordsFromMs)
	}
	snap, _, reqs := cycle(t0+180, "paper-BTC-30m-skew")
	if ok, why := usable(snap, "paper-BTC-30m-skew"); !ok || len(fundingReads(reqs, "ETH")) != 0 {
		t.Fatalf("BTC cycle usable=%t (%s), ETH reads %+v", ok, why, fundingReads(reqs, "ETH"))
	}
	snap, _, reqs = cycle(t0+240, "paper-ETH-1h-skew")
	if ok, why := usable(snap, "paper-ETH-1h-skew"); !ok || len(fundingReads(reqs, "ETH")) != 0 {
		t.Fatalf("ETH after a cycle that did not need it: usable=%t (%s), reads %+v; want its fresh records reused", ok, why, fundingReads(reqs, "ETH"))
	}
	snap, _, _ = cycle(t0+300, "paper-BTC-1h-skew", "paper-BTC-30m-neutral")
	if ok, why := usable(snap, "paper-BTC-1h-skew"); !ok {
		t.Fatalf("a wider due reader is held after its preparation: %s", why)
	}
	if ok, why := usable(snap, "paper-BTC-30m-neutral"); !ok {
		t.Fatalf("the scalar reader is held: %s", why)
	}
}

func TestFeedFundingStarvedLoadBecomesVisible(t *testing.T) {
	t0 := rdBase()
	f := newFCDeepFixture(t, time.Unix(t0-2000, 0).UTC())
	candle, _ := hlInfoWeight("candleSnapshot", 3)
	mids, _ := hlInfoWeight("allMids", 0)
	f.enforce(60, 60, 25*candle+mids, 0, 200)
	var alerts []string
	f.sealer.rateLimitSend = func(msg string) { alerts = append(alerts, msg) }
	for d := t0; d < t0+1500; d += 75 {
		doc, _ := f.seal(d)
		for _, k := range doc.Keys {
			if !k.Readiness.Ready {
				t.Fatalf("deadline %d: funding demand took the credit of candle key %s|%s", d, k.Symbol, k.Timeframe)
			}
		}
		refused := f.ledger.status().Weight.Totals.Refused
		if d < t0+int64(feedFundingDeferralLimit/time.Second) && refused != 0 {
			t.Fatalf("deadline +%ds counts %d refusals, want expected deferral to stay apart from refusal", d-t0, refused)
		}
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "WEIGHT REFUSED") {
		t.Fatalf("alerts = %q, want one refusal once the deferral limit passed", alerts)
	}
}
