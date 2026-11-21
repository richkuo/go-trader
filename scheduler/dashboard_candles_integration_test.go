package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type dcEnv struct {
	t   *testing.T
	ss  *StatusServer
	dir string
}

type dcResp struct {
	code   int
	header http.Header
	body   candleDisplayResponse
	errMsg struct {
		Code         string              `json:"code"`
		Error        string              `json:"error"`
		Capabilities *candleCapabilities `json:"capabilities"`
	}
}

func newDCEnv(t *testing.T, store *StateStore, cfgs ...StrategyConfig) *dcEnv {
	t.Helper()
	var mu sync.RWMutex
	e := &dcEnv{t: t, dir: t.TempDir()}
	e.ss = NewStatusServer(NewAppState(), &mu, "", cfgs, store)
	return e
}

func (e *dcEnv) archive(spec testArchiveSpec) string {
	e.t.Helper()
	id := writeTestCandleArchive(e.t, e.dir, spec)
	if e.ss.candles.archive.enabled() {
		e.ss.candles.archive.check(context.Background())
	} else {
		attachTestCandleArchive(e.t, e.ss, e.dir)
	}
	if g := e.ss.candles.archive.generation(); g == nil || g.ID != id {
		e.t.Fatalf("generation %s was not admitted: %+v", id, e.ss.candles.archive.status())
	}
	return id
}

func (e *dcEnv) get(id string, q url.Values, hdr map[string]string) dcResp {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/strategies/"+url.PathEscape(id)+"/candles?"+q.Encode(), nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ss.handleAPIStrategy(rec, req)
	out := dcResp{code: rec.Code, header: rec.Header()}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out.body); err != nil {
			e.t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	} else if rec.Code != http.StatusNotModified {
		json.Unmarshal(rec.Body.Bytes(), &out.errMsg)
	}
	return out
}

func dcHLStrategy(id, symbol, tf string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Symbol: symbol, Timeframe: tf,
		Args: []string{"momentum", symbol, tf, "--mode=paper"}}
}

func dcSnapshot(evaluation string, gen uint64, sealed time.Time, entries map[marketFeedKey]*marketSnapshotKey) *marketSnapshot {
	snap := degradedSharedSnapshot(sealed.Unix(), sealed)
	snap.EvaluationID = evaluation
	snap.ConfigGeneration = gen
	snap.SealedAt = sealed
	for k, v := range entries {
		snap.keys[k] = v
	}
	return snap
}

func dcFeedBars(from time.Time, n int, step time.Duration) []feedBar {
	out := make([]feedBar, 0, n)
	for i := 0; i < n; i++ {
		open := from.Add(time.Duration(i) * step).UnixMilli()
		out = append(out, feedBar{OpenMs: open, CloseMs: open + step.Milliseconds() - 1, HasClose: true, Open: 100, High: 110, Low: 95, Close: 105, Volume: 7})
	}
	return out
}

func dcCheckContiguous(t *testing.T, bars []UICandle, step int64) {
	t.Helper()
	for i := 1; i < len(bars); i++ {
		if bars[i].Time-bars[i-1].Time != step {
			t.Fatalf("bars %d and %d are %d seconds apart, want %d", i-1, i, bars[i].Time-bars[i-1].Time, step)
		}
	}
}

func TestDashboardCandlesArchiveTimeframesPagingAndCursorTurnover(t *testing.T) {
	e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
	latest := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	start := latest.Add(-999 * time.Hour)
	four := start.Truncate(4 * time.Hour)
	e.archive(testArchiveSpec{Series: []testArchiveSeries{
		{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(start, latest, time.Hour), ChunkBars: 64},
		{ID: hlTestIdentity("ETH", "4h"), Bars: testArchiveBars(four, latest.Truncate(4*time.Hour).Add(-4*time.Hour), 4*time.Hour), HistoryStart: true},
	}})

	one := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "limit": {"5"}}, nil)
	four4 := e.get("hl-eth", url.Values{"timeframe": {"4h"}, "limit": {"5"}}, nil)
	if one.code != 200 || four4.code != 200 {
		t.Fatalf("timeframe reads = %d / %d", one.code, four4.code)
	}
	if one.body.Timeframe != "1h" || four4.body.Timeframe != "4h" || one.body.BarSeconds != 3600 || four4.body.BarSeconds != 14400 {
		t.Fatalf("timeframe fields = %s/%d and %s/%d", one.body.Timeframe, one.body.BarSeconds, four4.body.Timeframe, four4.body.BarSeconds)
	}
	k1, k4 := one.header.Get("X-Go-Trader-Candle-Key"), four4.header.Get("X-Go-Trader-Candle-Key")
	if k1 == "" || k4 == "" || k1 == k4 {
		t.Fatalf("work keys %q and %q must differ", k1, k4)
	}
	if one.body.Origin != candleOriginArchive || one.body.SourceClass != candleSourceClassMarket || one.body.Identity == nil || one.body.Identity.Network != candleNetworkMainnet {
		t.Fatalf("1h response origin %q class %q identity %+v", one.body.Origin, one.body.SourceClass, one.body.Identity)
	}
	if len(one.body.Candles) != 5 || one.body.Candles[4].Time != latest.Unix() {
		t.Fatalf("latest 1h window = %+v", one.body.Candles)
	}
	if one.body.Capabilities.DefaultTimeframe != "1h" || len(one.body.Capabilities.Timeframes) != 2 || one.body.Capabilities.Timeframes[0].Timeframe != "1h" {
		t.Fatalf("capabilities = %+v", one.body.Capabilities)
	}
	if def := e.get("hl-eth", url.Values{"limit": {"5"}}, nil); def.code != 200 || def.body.Timeframe != "1h" {
		t.Fatalf("default timeframe = %d %s", def.code, def.body.Timeframe)
	}

	if r := e.get("hl-eth", url.Values{"timeframe": {"7m"}}, nil); r.code != 400 || r.errMsg.Code != "timeframe_invalid" {
		t.Fatalf("7m = %d %q", r.code, r.errMsg.Code)
	}
	if r := e.get("hl-eth", url.Values{"timeframe": {"15m"}}, nil); r.code != 400 || r.errMsg.Code != "timeframe_unsupported" || r.errMsg.Capabilities == nil {
		t.Fatalf("15m = %d %q caps %v", r.code, r.errMsg.Code, r.errMsg.Capabilities)
	}
	for name, q := range map[string]url.Values{
		"bad from":       {"from": {"yesterday"}},
		"to before from": {"from": {strconv.FormatInt(latest.Unix(), 10)}, "to": {strconv.FormatInt(start.Unix(), 10)}},
		"before and to":  {"before": {"abc"}, "to": {"5"}},
		"garbage cursor": {"before": {"!!"}},
		"zero limit":     {"limit": {"0"}},
	} {
		if r := e.get("hl-eth", q, nil); r.code != 400 {
			t.Fatalf("%s = %d, want 400", name, r.code)
		}
	}

	from := latest.Add(-50 * time.Hour).Unix()
	to := latest.Add(-40 * time.Hour).Unix()
	bounded := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "from": {strconv.FormatInt(from, 10)}, "to": {strconv.FormatInt(to, 10)}}, nil)
	if bounded.code != 200 || len(bounded.body.Candles) != 11 || bounded.body.Candles[0].Time != from || bounded.body.Candles[10].Time != to {
		t.Fatalf("inclusive bounds returned %d bars (%d)", len(bounded.body.Candles), bounded.code)
	}
	if big := e.get("hl-eth", url.Values{"limit": {"5000"}}, nil); big.code != 200 || len(big.body.Candles) != candleDisplayMaxLimit {
		t.Fatalf("limit cap returned %d bars", len(big.body.Candles))
	}

	first := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "limit": {"300"}}, nil)
	seen := map[int64]bool{}
	all := append([]UICandle(nil), first.body.Candles...)
	for _, c := range first.body.Candles {
		seen[c.Time] = true
	}
	cursor := first.body.Page.Older
	pages := 1
	var expired string
	for cursor != "" {
		if pages == 2 {
			expired = cursor
		}
		next := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "limit": {"300"}, "before": {cursor}}, nil)
		if next.code != 200 {
			t.Fatalf("page %d = %d %s", pages+1, next.code, next.errMsg.Error)
		}
		if len(next.body.Candles) == 0 || next.body.Candles[len(next.body.Candles)-1].Time != all[0].Time-3600 {
			t.Fatalf("page %d does not end one bar before the previous page", pages+1)
		}
		for _, c := range next.body.Candles {
			if seen[c.Time] {
				t.Fatalf("opening time %d appears twice", c.Time)
			}
			seen[c.Time] = true
		}
		all = append(append([]UICandle(nil), next.body.Candles...), all...)
		cursor = next.body.Page.Older
		pages++
		if cursor == "" && next.body.Page.Boundary != candleBoundaryArchiveStart {
			t.Fatalf("paging ended with boundary %q, want %q", next.body.Page.Boundary, candleBoundaryArchiveStart)
		}
		if pages > 10 {
			t.Fatal("paging did not stop")
		}
	}
	dcCheckContiguous(t, all, 3600)
	if pages != 4 || len(all) != 1000 || all[0].Time != start.Unix() {
		t.Fatalf("paged %d pages, %d bars, first %d (want 4 pages, 1000 bars from %d)", pages, len(all), all[0].Time, start.Unix())
	}

	etag := first.header.Get("ETag")
	if again := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "limit": {"300"}}, map[string]string{"If-None-Match": etag}); again.code != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", again.code)
	}

	e.archive(testArchiveSpec{Series: []testArchiveSeries{
		{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(start, latest, time.Hour)},
	}})
	if r := e.get("hl-eth", url.Values{"timeframe": {"1h"}, "limit": {"300"}, "before": {expired}}, nil); r.code != http.StatusConflict || r.errMsg.Code != "cursor_expired" {
		t.Fatalf("cursor after turnover = %d %q, want 409 cursor_expired", r.code, r.errMsg.Code)
	}
	if r := e.get("hl-eth", url.Values{"timeframe": {"4h"}}, nil); r.code != 400 || r.errMsg.Code != "timeframe_unsupported" {
		t.Fatalf("4h after the new generation dropped it = %d %q", r.code, r.errMsg.Code)
	}
}

func TestDashboardCandlesArchiveGapStopsPaging(t *testing.T) {
	e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
	latest := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	old := testArchiveBars(latest.Add(-400*time.Hour), latest.Add(-201*time.Hour), time.Hour)
	recent := testArchiveBars(latest.Add(-199*time.Hour), latest, time.Hour)
	e.archive(testArchiveSpec{Series: []testArchiveSeries{{ID: hlTestIdentity("ETH", "1h"), Bars: append(old, recent...)}}})
	first := e.get("hl-eth", url.Values{"limit": {"150"}}, nil)
	if first.code != 200 || first.body.Page.Older == "" {
		t.Fatalf("first page = %d older %q", first.code, first.body.Page.Older)
	}
	second := e.get("hl-eth", url.Values{"limit": {"150"}, "before": {first.body.Page.Older}}, nil)
	if second.code != 200 || len(second.body.Candles) != 50 || second.body.Page.Older != "" || second.body.Page.Boundary != candleBoundarySourceGap {
		t.Fatalf("second page = %d bars %d older %q boundary %q", second.code, len(second.body.Candles), second.body.Page.Older, second.body.Page.Boundary)
	}
}

func TestDashboardCandlesPublishedFeedBarsAndOlderArchivePages(t *testing.T) {
	e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
	now := time.Now().UTC()
	forming := now.Truncate(time.Hour)
	feedStart := forming.Add(-99 * time.Hour)
	key := feedKeyFor("ETH", "1h")
	bars := dcFeedBars(feedStart, 100, time.Hour)
	entry := &marketSnapshotKey{Key: key, IntervalMs: 3_600_000, Bars: bars, Readiness: feedKeyReadiness{Key: key, Ready: true, Required: 100, Status: feedStatusReady}}
	reqs := cycleMarketRequirements{Keys: []cycleMarketRequirement{{Key: key, Required: 100}}}
	e.ss.PublishMarketSnapshot(dcSnapshot("eval-1", 3, now, map[marketFeedKey]*marketSnapshotKey{key: entry}), reqs, true)
	bars[50].Close = 999

	r := e.get("hl-eth", url.Values{"limit": {"60"}}, nil)
	if r.code != 200 || r.body.Origin != candleOriginPublished || r.body.Source != "hyperliquid:feed:primary" || len(r.body.Candles) != 60 {
		t.Fatalf("published read = %d origin %q source %q bars %d", r.code, r.body.Origin, r.body.Source, len(r.body.Candles))
	}
	if r.body.FormingTime != forming.Unix() || r.body.Candles[59].Time != forming.Unix() {
		t.Fatalf("forming_time = %d, newest bar %d, want %d", r.body.FormingTime, r.body.Candles[59].Time, forming.Unix())
	}
	for _, c := range r.body.Candles {
		if c.Close == 999 {
			t.Fatal("a change to the sealed bars after publication reached the dashboard")
		}
	}
	if r.body.Freshness != candleFreshnessFresh || r.body.Generation != "feed-3:eval-1" {
		t.Fatalf("freshness %q generation %q", r.body.Freshness, r.body.Generation)
	}
	older := e.get("hl-eth", url.Values{"limit": {"60"}, "before": {r.body.Page.Older}}, nil)
	if older.code != 200 || older.body.Origin != candleOriginPublished || len(older.body.Candles) != 40 || older.body.Page.Boundary != candleBoundaryReadBound {
		t.Fatalf("older feed page = %d origin %q bars %d boundary %q", older.code, older.body.Origin, len(older.body.Candles), older.body.Page.Boundary)
	}

	e.archive(testArchiveSpec{Series: []testArchiveSeries{{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(feedStart.Add(-300*time.Hour), feedStart.Add(-time.Hour), time.Hour)}}})
	r = e.get("hl-eth", url.Values{"limit": {"100"}}, nil)
	if r.code != 200 || r.body.Origin != candleOriginPublished || r.body.Page.Older == "" {
		t.Fatalf("published read with an archive = %d origin %q older %q", r.code, r.body.Origin, r.body.Page.Older)
	}
	joined := e.get("hl-eth", url.Values{"limit": {"100"}, "before": {r.body.Page.Older}}, nil)
	if joined.code != 200 || joined.body.Origin != candleOriginArchive || len(joined.body.Candles) != 100 || joined.body.Candles[99].Time != feedStart.Add(-time.Hour).Unix() {
		t.Fatalf("archive page after feed bars = %d origin %q bars %d", joined.code, joined.body.Origin, len(joined.body.Candles))
	}
	if joined.body.SeriesToken != r.body.SeriesToken || joined.body.SourceClass != candleSourceClassMarket {
		t.Fatalf("feed and archive pages of one identity must share the market series token")
	}

	sealedAt := now
	e.ss.PublishMarketSnapshot(dcSnapshot("eval-2", 3, now.Add(time.Minute), nil), cycleMarketRequirements{}, true)
	if got := e.ss.candles.published.get(hlTestIdentity("ETH", "1h")); got == nil || !got.SealedAt.Equal(sealedAt) || got.Latest.Evaluation != "eval-1" {
		t.Fatalf("a cycle that did not ask for the key changed its age: %+v", got)
	}
	unready := &marketSnapshotKey{Key: key, IntervalMs: 3_600_000, Bars: nil, Readiness: feedKeyReadiness{Key: key, Status: feedStatusRateLimited, Detail: "host paused"}}
	e.ss.PublishMarketSnapshot(dcSnapshot("eval-3", 3, now.Add(2*time.Minute), map[marketFeedKey]*marketSnapshotKey{key: unready}), reqs, false)
	got := e.ss.candles.published.get(hlTestIdentity("ETH", "1h"))
	if got == nil || len(got.Bars) != 100 || !got.SealedAt.Equal(sealedAt) || got.Latest.Ready || got.Latest.Status != string(feedStatusRateLimited) {
		t.Fatalf("an unready key must keep the last ready bars at their age and record its status: %+v", got)
	}
	e.ss.PublishMarketSnapshot(degradedSharedSnapshot(now.Add(3*time.Minute).Unix(), now.Add(3*time.Minute)), reqs, true)
	got = e.ss.candles.published.get(hlTestIdentity("ETH", "1h"))
	if got == nil || len(got.Bars) != 100 || got.Latest.Status != "unavailable" {
		t.Fatalf("a degraded snapshot must mark the requested key unavailable and keep its bars: %+v", got)
	}
	stillServed := e.get("hl-eth", url.Values{"limit": {"10"}}, nil)
	if stillServed.code != 200 || stillServed.body.AsOf != sealedAt.Format(time.RFC3339) {
		t.Fatalf("display after a degraded cycle = %d as_of %q, want the original age %q", stillServed.code, stillServed.body.AsOf, sealedAt.Format(time.RFC3339))
	}

	testnet := dcHLStrategy("hl-eth", "ETH", "1h")
	t.Setenv("HYPERLIQUID_TESTNET", "1")
	e.ss.UpdateStrategies([]StrategyConfig{testnet})
	tn := e.get("hl-eth", url.Values{"limit": {"10"}}, nil)
	if tn.code != 200 || tn.body.Origin == candleOriginPublished || tn.body.Origin == candleOriginArchive {
		t.Fatalf("a testnet strategy read mainnet bars: %d origin %q", tn.code, tn.body.Origin)
	}
}

func TestDashboardCandlesSyntheticBarsStayInDisplay(t *testing.T) {
	db := openTestDB(t)
	store := singleFileStore(db)
	spot := StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Args: []string{"sma", "BTC/USDT", "1h", "--mode=paper"}}
	base := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	for i := 0; i < 10; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).Add(10 * time.Minute)
		if err := db.InsertTrade("spot-btc", Trade{StrategyID: "spot-btc", Timestamp: ts, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 100 + float64(i), Value: 100, TradeType: "spot"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertTrade("spot-btc", Trade{StrategyID: "spot-btc", Timestamp: base.Add(3 * time.Hour), Symbol: "ETH/USDT", Side: "buy", Quantity: 1, Price: 5000, Value: 5000, TradeType: "spot"}); err != nil {
		t.Fatal(err)
	}
	e := newDCEnv(t, store, spot)
	r := e.get("spot-btc", url.Values{}, nil)
	if r.code != 200 || r.body.SourceClass != candleSourceClassSynthetic || r.body.Source != "trades" || len(r.body.Candles) != 10 {
		t.Fatalf("synthetic read = %d class %q source %q bars %d", r.code, r.body.SourceClass, r.body.Source, len(r.body.Candles))
	}
	for _, c := range r.body.Candles {
		if c.High >= 5000 || c.Volume != 0 {
			t.Fatalf("bar %+v carries the other symbol's trade or a volume", c)
		}
	}
	if r.body.MarketAvailability == nil || r.body.MarketAvailability.Code != candleAvailSourceMissing || r.body.Page.Boundary != candleBoundaryHistoryStart {
		t.Fatalf("synthetic availability %+v boundary %q", r.body.MarketAvailability, r.body.Page.Boundary)
	}
	if bad := e.get("spot-btc", url.Values{"timeframe": {"4h"}}, nil); bad.code != 400 || bad.errMsg.Code != "timeframe_unsupported" {
		t.Fatalf("synthetic bars at another timeframe = %d %q", bad.code, bad.errMsg.Code)
	}
	preview, perr := e.ss.candles.previewBars(context.Background(), spot, 0, 0, 400)
	if perr == nil || perr.Status != http.StatusServiceUnavailable || perr.Code != candleAvailSourceMissing || len(preview.Bars) != 0 {
		t.Fatalf("preview after a cached synthetic display read = %+v %+v", preview, perr)
	}

	for i := 0; i < 501; i++ {
		ts := base.Add(-100 * time.Hour).Add(time.Duration(i) * 7 * time.Minute)
		if err := db.InsertTrade("spot-btc", Trade{StrategyID: "spot-btc", Timestamp: ts, Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "spot"}); err != nil {
			t.Fatal(err)
		}
	}
	e.ss.UpdateStrategies([]StrategyConfig{spot})
	bound := e.get("spot-btc", url.Values{"limit": {"1000"}}, nil)
	if bound.code != 200 || !bound.body.Page.ReadBound || bound.body.Page.Older == "" {
		t.Fatalf("501-trade history = %d read_bound %v older %q", bound.code, bound.body.Page.ReadBound, bound.body.Page.Older)
	}
	firstBar := bound.body.Candles[0].Time
	older := e.get("spot-btc", url.Values{"limit": {"1000"}, "before": {bound.body.Page.Older}}, nil)
	if older.code != 200 || len(older.body.Candles) == 0 || older.body.Candles[len(older.body.Candles)-1].Time >= firstBar {
		t.Fatalf("older synthetic page = %d bars %d", older.code, len(older.body.Candles))
	}

	dir := t.TempDir()
	mk := func(id, src string) StrategyConfig {
		return StrategyConfig{ID: id, StorageStrategyID: "btc", PaperSource: src, Type: "spot", Platform: "binanceus", Symbol: "BTC/USDT", Timeframe: "1h",
			Args: []string{"sma", "BTC/USDT", "1h", "--mode=paper"}}
	}
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}, {ID: "s2", DBFile: filepath.Join(dir, "s2.db")}},
		Strategies:   []StrategyConfig{mk("s1-btc", "s1"), mk("s2-btc", "s2")}}
	split := openSplitStore(t, cfg)
	for i, id := range []string{"s1-btc", "s2-btc"} {
		if err := split.InsertTrade(id, Trade{StrategyID: id, Timestamp: base.Add(time.Duration(i) * 5 * time.Hour), Symbol: "BTC/USDT", Side: "buy", Quantity: 1, Price: 100 + float64(i), Value: 100, TradeType: "spot"}); err != nil {
			t.Fatal(err)
		}
	}
	se := newDCEnv(t, split, cfg.Strategies...)
	a := se.get("s1-btc", url.Values{}, nil)
	b := se.get("s2-btc", url.Values{}, nil)
	if a.code != 200 || b.code != 200 || len(a.body.Candles) != 1 || len(b.body.Candles) != 1 || a.body.Candles[0].Close != 100 || b.body.Candles[0].Close != 101 {
		t.Fatalf("paper sources with one storage id read each other's trades: %+v / %+v", a.body.Candles, b.body.Candles)
	}
	if a.body.SeriesToken == b.body.SeriesToken {
		t.Fatal("two storage partitions share a synthetic series token")
	}
}

func TestDashboardCandlesSharedBoundCoalescingAndStrategyChange(t *testing.T) {
	e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
	latest := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	e.archive(testArchiveSpec{Series: []testArchiveSeries{{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(latest.Add(-2000*time.Hour), latest, time.Hour), ChunkBars: 256}}})

	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.get("hl-eth", url.Values{"limit": {"700"}}, nil).code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("concurrent read %d = %d", i, c)
		}
	}
	if reads := e.ss.candles.status().SourceReads; reads != 1 {
		t.Fatalf("12 concurrent identical reads ran the archive read %d times, want 1", reads)
	}

	release := make(chan struct{})
	started := make(chan struct{}, candleServiceSlots+candleServiceQueue)
	var hold sync.WaitGroup
	total := candleServiceSlots + candleServiceQueue
	for i := 0; i < total; i++ {
		hold.Add(1)
		go func(i int) {
			defer hold.Done()
			e.ss.candles.execute(context.Background(), fmt.Sprintf("hold|%d", i), false, func(context.Context) (candlePage, error) {
				started <- struct{}{}
				<-release
				return candlePage{}, nil
			})
		}(i)
	}
	for i := 0; i < candleServiceSlots; i++ {
		<-started
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.ss.candles.queue) < total && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	busy := e.get("hl-eth", url.Values{"limit": {"10"}}, nil)
	if busy.code != http.StatusTooManyRequests || busy.errMsg.Code != "busy" {
		t.Fatalf("request slots+queue+1 = %d %q, want 429 busy", busy.code, busy.errMsg.Code)
	}
	if ra, err := strconv.Atoi(busy.header.Get("Retry-After")); err != nil || ra <= 0 {
		t.Fatalf("Retry-After = %q", busy.header.Get("Retry-After"))
	}
	status := httptest.NewRecorder()
	e.ss.handleStatus(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if status.Code != 200 {
		t.Fatalf("/status while the candle bound is full = %d", status.Code)
	}
	close(release)
	hold.Wait()
	if e.ss.candles.status().Refused == 0 {
		t.Fatal("the refusal was not counted")
	}

	old := dcHLStrategy("hl-eth", "ETH", "1h")
	e.ss.UpdateStrategies([]StrategyConfig{dcHLStrategy("hl-eth", "ETH", "4h")})
	_, _, serr := e.ss.candles.displayBars(context.Background(), old, candleDisplayRequest{Limit: 10})
	if serr == nil || serr.Status != http.StatusConflict || serr.Code != "strategy_changed" {
		t.Fatalf("a read admitted before a strategy reload = %+v, want 409 strategy_changed", serr)
	}
}

func TestDashboardCandlesArchiveLoaderRefusalsAndFailedRefresh(t *testing.T) {
	latest := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	good := func() testArchiveSeries {
		return testArchiveSeries{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(latest.Add(-99*time.Hour), latest, time.Hour)}
	}
	cases := map[string]func(dir string) testArchiveSpec{
		"future publish time": func(string) testArchiveSpec {
			return testArchiveSpec{PublishedAt: time.Now().Add(time.Hour), Series: []testArchiveSeries{good()}}
		},
		"duplicate identity": func(string) testArchiveSpec { return testArchiveSpec{Series: []testArchiveSeries{good(), good()}} },
		"timestamp not open": func(string) testArchiveSpec {
			s := good()
			s.Raw = func(m *candleArchiveSeriesManifest) { m.Timestamp = "close" }
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
		"wrong interval": func(string) testArchiveSpec {
			s := good()
			s.Raw = func(m *candleArchiveSeriesManifest) { m.IntervalMs = 60_000 }
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
		"undeclared gap": func(string) testArchiveSpec {
			s := good()
			s.Bars = append(append([]UICandle(nil), s.Bars[:10]...), s.Bars[12:]...)
			s.Raw = func(m *candleArchiveSeriesManifest) {
				m.Segments = []candleArchiveSegment{{FirstOpenMs: s.Bars[0].Time * 1000, LastOpenMs: s.Bars[len(s.Bars)-1].Time * 1000, Bars: int64(len(s.Bars))}}
			}
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
		"unfinalized last bar": func(string) testArchiveSpec {
			s := good()
			s.Bars = append(s.Bars, UICandle{Time: latest.Add(time.Hour).Unix(), Open: 100, High: 120, Low: 90, Close: 105, Volume: 1})
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
		"finalized pointer": func(string) testArchiveSpec {
			s := good()
			s.Raw = func(m *candleArchiveSeriesManifest) { m.FinalizedThroughOpenMs -= 3_600_000 }
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
		"refused venue": func(string) testArchiveSpec {
			s := good()
			s.ID = candleIdentity{Venue: candleVenueOKX, Network: candleNetworkLive, InstrumentClass: candleClassPerps, Symbol: "BTC-USDT-SWAP", Timeframe: "1h"}
			return testArchiveSpec{Series: []testArchiveSeries{s}}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			id := writeTestCandleArchive(t, dir, mk(dir))
			if _, err := loadCandleArchiveGeneration(context.Background(), dir, id, candleArchiveLimits{}, time.Now()); err == nil {
				t.Fatalf("%s was admitted", name)
			}
		})
	}
	t.Run("file mutations", func(t *testing.T) {
		for _, mut := range []string{"size", "digest", "symlink", "id", "limit", "unknown field"} {
			dir := t.TempDir()
			id := writeTestCandleArchive(t, dir, testArchiveSpec{Series: []testArchiveSeries{good()}})
			gen := filepath.Join(dir, candleArchiveGenerationsDir, id)
			series := filepath.Join(gen, "series", "s000.bars")
			limits := candleArchiveLimits{}
			switch mut {
			case "size":
				f, _ := os.OpenFile(series, os.O_APPEND|os.O_WRONLY, 0)
				f.Write([]byte{1})
				f.Close()
			case "digest":
				data, _ := os.ReadFile(series)
				data[100] ^= 1
				os.WriteFile(series, data, 0o644)
			case "symlink":
				os.Rename(series, series+".real")
				os.Symlink(series+".real", series)
			case "id":
				raw, _ := os.ReadFile(filepath.Join(gen, candleArchiveManifestName))
				os.WriteFile(filepath.Join(gen, candleArchiveManifestName), append(raw, ' '), 0o644)
			case "limit":
				limits.MaxBytes = 100
			case "unknown field":
				raw, _ := os.ReadFile(filepath.Join(gen, candleArchiveManifestName))
				var m map[string]json.RawMessage
				json.Unmarshal(raw, &m)
				m["extra"] = json.RawMessage(`1`)
				raw, _ = json.Marshal(m)
				os.WriteFile(filepath.Join(gen, candleArchiveManifestName), raw, 0o644)
				var published int64
				json.Unmarshal(m["published_at_ms"], &published)
				next := candleArchiveGenerationID(published, raw)
				os.Rename(gen, filepath.Join(dir, candleArchiveGenerationsDir, next))
				id = next
			}
			if _, err := loadCandleArchiveGeneration(context.Background(), dir, id, limits, time.Now()); err == nil {
				t.Fatalf("mutation %s was admitted", mut)
			}
		}
	})
	t.Run("failed refresh keeps the previous generation", func(t *testing.T) {
		e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
		first := e.archive(testArchiveSpec{Series: []testArchiveSeries{good()}})
		bad := good()
		bad.Bars[5].High = 1
		writeTestCandleArchive(t, e.dir, testArchiveSpec{Series: []testArchiveSeries{bad}})
		e.ss.candles.archive.check(context.Background())
		st := e.ss.candles.archive.status()
		if st.Generation != first || st.State != candleArchiveStateReady || st.LastRefreshErr == "" {
			t.Fatalf("after a refused refresh: %+v", st)
		}
		if r := e.get("hl-eth", url.Values{"limit": {"5"}}, nil); r.code != 200 || r.body.Generation != first {
			t.Fatalf("display after a refused refresh = %d generation %q", r.code, r.body.Generation)
		}
		os.Remove(filepath.Join(e.dir, candleArchiveCurrentName))
		e.ss.candles.archive.check(context.Background())
		if st := e.ss.candles.archive.status(); st.Generation != first {
			t.Fatalf("a missing CURRENT dropped the admitted generation: %+v", st)
		}
	})
	t.Run("expired generation is display-only", func(t *testing.T) {
		e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
		s := good()
		s.Bars = testArchiveBars(latest.Add(-99*time.Hour), latest.Add(-3*time.Hour), time.Hour)
		e.archive(testArchiveSpec{PublishedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour), Series: []testArchiveSeries{s}})
		r := e.get("hl-eth", url.Values{"limit": {"5"}}, nil)
		if r.code != 200 || r.body.Freshness != candleFreshnessStale || len(r.body.Candles) != 5 {
			t.Fatalf("expired display = %d freshness %q bars %d", r.code, r.body.Freshness, len(r.body.Candles))
		}
		if _, perr := e.ss.candles.previewBars(context.Background(), dcHLStrategy("hl-eth", "ETH", "1h"), 0, 0, 100); perr == nil || perr.Status != http.StatusServiceUnavailable {
			t.Fatalf("preview on an expired generation = %+v", perr)
		}
	})
}

func TestDashboardCandlesArchiveLoaderRetriesTransientRefusals(t *testing.T) {
	latest := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	good := func() testArchiveSeries {
		return testArchiveSeries{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(latest.Add(-99*time.Hour), latest, time.Hour)}
	}
	swaps := func(l *candleArchiveLoader) int {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.swaps
	}
	t.Run("a publish time ahead of the clock is retried", func(t *testing.T) {
		e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
		published := time.Now().UTC().Add(-time.Minute)
		e.ss.ConfigureDashboardCandles(&DashboardCandlesConfig{ArchiveDir: e.dir})
		l := e.ss.candles.archive
		clock := published.Add(-3 * time.Second)
		l.now = func() time.Time { return clock }
		id := writeTestCandleArchive(t, e.dir, testArchiveSpec{PublishedAt: published, Series: []testArchiveSeries{good()}})
		l.check(context.Background())
		if st := l.status(); st.Generation != "" || st.LastRefreshErr == "" {
			t.Fatalf("tick 1 with the clock 3 s behind the publish time: %+v", st)
		}
		clock = published.Add(time.Second)
		l.check(context.Background())
		if st := l.status(); st.Generation != id || st.State != candleArchiveStateReady {
			t.Fatalf("a later tick with no new publish must admit %s: %+v", id, st)
		}
	})
	t.Run("a bad chunk digest is refused once", func(t *testing.T) {
		e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
		e.ss.ConfigureDashboardCandles(&DashboardCandlesConfig{ArchiveDir: e.dir})
		l := e.ss.candles.archive
		id := writeTestCandleArchive(t, e.dir, testArchiveSpec{Series: []testArchiveSeries{good()}})
		series := filepath.Join(e.dir, candleArchiveGenerationsDir, id, "series", "s000.bars")
		data, err := os.ReadFile(series)
		if err != nil {
			t.Fatal(err)
		}
		data[100] ^= 1
		if err := os.WriteFile(series, data, 0o644); err != nil {
			t.Fatal(err)
		}
		l.check(context.Background())
		if st := l.status(); st.Generation != "" || !strings.Contains(st.LastRefreshErr, "digest mismatch") {
			t.Fatalf("a bad chunk digest: %+v", st)
		}
		data[100] ^= 1
		if err := os.WriteFile(series, data, 0o644); err != nil {
			t.Fatal(err)
		}
		l.check(context.Background())
		if st := l.status(); st.Generation != "" {
			t.Fatalf("a generation refused for its content was read again on the next tick: %+v", st)
		}
	})
	t.Run("an admitted CURRENT settles without a reload", func(t *testing.T) {
		e := newDCEnv(t, nil, dcHLStrategy("hl-eth", "ETH", "1h"))
		id := e.archive(testArchiveSpec{Series: []testArchiveSeries{good()}})
		l := e.ss.candles.archive
		before := swaps(l)
		l.check(context.Background())
		l.check(context.Background())
		if st := l.status(); st.Generation != id || swaps(l) != before || before != 1 {
			t.Fatalf("checks of an admitted CURRENT: generation %q swaps %d, want %q and %d", st.Generation, swaps(l), id, before)
		}
	})
}

func TestDashboardCandlesPreviewUsesClosedBars(t *testing.T) {
	now := time.Now().UTC()
	forming := now.Truncate(time.Hour)
	key := feedKeyFor("ETH", "1h")
	sc := dcHLStrategy("hl-eth", "ETH", "1h")
	reqs := cycleMarketRequirements{Keys: []cycleMarketRequirement{{Key: key, Required: 100}}}
	publish := func(e *dcEnv, bars []feedBar) {
		entry := &marketSnapshotKey{Key: key, IntervalMs: 3_600_000, Bars: bars, Readiness: feedKeyReadiness{Key: key, Ready: true, Required: 100, Status: feedStatusReady}}
		e.ss.PublishMarketSnapshot(dcSnapshot("eval-1", 3, now, map[marketFeedKey]*marketSnapshotKey{key: entry}), reqs, true)
	}
	t.Run("the forming feed bar is absent", func(t *testing.T) {
		e := newDCEnv(t, nil, sc)
		publish(e, dcFeedBars(forming.Add(-99*time.Hour), 100, time.Hour))
		if got := e.ss.candles.published.get(hlTestIdentity("ETH", "1h")); got == nil || got.FormingOpen != forming.Unix() {
			t.Fatalf("the fixture must hold a forming bar at %d: %+v", forming.Unix(), got)
		}
		preview, perr := e.ss.candles.previewBars(context.Background(), sc, 0, 0, 400)
		if perr != nil || preview.Origin != candleOriginPublished || len(preview.Bars) != 99 || preview.Bars[98].Time != forming.Add(-time.Hour).Unix() {
			t.Fatalf("preview with a forming feed bar = %+v origin %q bars %d", perr, preview.Origin, len(preview.Bars))
		}
	})
	t.Run("a feed series with no forming bar keeps every bar", func(t *testing.T) {
		e := newDCEnv(t, nil, sc)
		publish(e, dcFeedBars(forming.Add(-100*time.Hour), 100, time.Hour))
		if got := e.ss.candles.published.get(hlTestIdentity("ETH", "1h")); got == nil || got.FormingOpen != 0 {
			t.Fatalf("the fixture must hold no forming bar: %+v", got)
		}
		preview, perr := e.ss.candles.previewBars(context.Background(), sc, 0, 0, 400)
		if perr != nil || preview.Origin != candleOriginPublished || len(preview.Bars) != 100 || preview.Bars[99].Time != forming.Add(-time.Hour).Unix() {
			t.Fatalf("preview with closed feed bars = %+v origin %q bars %d", perr, preview.Origin, len(preview.Bars))
		}
	})
	t.Run("the archive origin keeps every finalized bar", func(t *testing.T) {
		e := newDCEnv(t, nil, sc)
		latest := forming.Add(-2 * time.Hour)
		e.archive(testArchiveSpec{Series: []testArchiveSeries{{ID: hlTestIdentity("ETH", "1h"), Bars: testArchiveBars(latest.Add(-99*time.Hour), latest, time.Hour)}}})
		preview, perr := e.ss.candles.previewBars(context.Background(), sc, 0, 0, 400)
		if perr != nil || preview.Origin != candleOriginArchive || len(preview.Bars) != 100 || preview.Bars[99].Time != latest.Unix() {
			t.Fatalf("preview from the archive = %+v origin %q bars %d", perr, preview.Origin, len(preview.Bars))
		}
	})
}
