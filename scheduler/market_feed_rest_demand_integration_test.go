package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var rdCoins = []string{"BNB", "BTC", "ETH", "HYPE", "SOL"}

type rdRequest struct {
	Type     string
	Coin     string
	Interval string
	Start    int64
	End      int64
	At       time.Time
	Rows     int
}

type rdVenue struct {
	srv   *httptest.Server
	clock *hlLotTestClock
	mu    sync.Mutex
	reqs  []rdRequest
	fail  map[string]bool
	mids  int

	fundingLag      time.Duration
	fundingConflict map[string]int64
	fundingDrop     map[string]int64
	fundingFail     map[string]bool
}

func newRDVenue(t *testing.T, clock *hlLotTestClock) *rdVenue {
	t.Helper()
	v := &rdVenue{clock: clock, fail: map[string]bool{}, mids: http.StatusOK,
		fundingConflict: map[string]int64{}, fundingDrop: map[string]int64{}, fundingFail: map[string]bool{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func rdBarClose(coin string, openMs, intervalMs int64) float64 {
	return 100 + float64((openMs/intervalMs)%17) + float64(len(coin))
}

func rdFormingClose(coin string, openMs, intervalMs int64, at time.Time) float64 {
	return rdBarClose(coin, openMs, intervalMs) + 0.25 + float64(at.Unix()%600)/1000
}

func rdFundingRate(coin string, hourMs int64) float64 {
	return float64((hourMs/hlFundingRecordStepMs)%13+int64(len(coin))) / 100000
}

func (v *rdVenue) record(r rdRequest) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reqs = append(v.reqs, r)
}

func (v *rdVenue) since(mark int, typ string) []rdRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []rdRequest
	for _, r := range v.reqs[mark:] {
		if typ == "" || r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

func (v *rdVenue) mark() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.reqs)
}

func (v *rdVenue) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type      string `json:"type"`
		Coin      string `json:"coin"`
		StartTime int64  `json:"startTime"`
		EndTime   int64  `json:"endTime"`
		Req       struct {
			Coin      string `json:"coin"`
			Interval  string `json:"interval"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		} `json:"req"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := v.clock.Now()
	w.Header().Set("Content-Type", "application/json")
	switch body.Type {
	case "candleSnapshot":
		coin, interval := body.Req.Coin, body.Req.Interval
		step, _ := hlCandleIntervalMs(interval)
		v.mu.Lock()
		fail := v.fail[coin+"|"+interval]
		v.mu.Unlock()
		if fail {
			v.record(rdRequest{Type: body.Type, Coin: coin, Interval: interval, Start: body.Req.StartTime, End: body.Req.EndTime, At: now})
			http.Error(w, "venue failure", http.StatusInternalServerError)
			return
		}
		nowMs := now.UnixMilli()
		rows := []map[string]any{}
		for open := (body.Req.StartTime + step - 1) / step * step; open <= body.Req.EndTime && open <= nowMs && len(rows) < 5000; open += step {
			c := rdBarClose(coin, open, step)
			if open+step > nowMs {
				c = rdFormingClose(coin, open, step, now)
			}
			rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": strconv.FormatFloat(c+5, 'f', -1, 64),
				"l": "90", "c": strconv.FormatFloat(c, 'f', -1, 64), "v": "5"})
		}
		v.record(rdRequest{Type: body.Type, Coin: coin, Interval: interval, Start: body.Req.StartTime, End: body.Req.EndTime, At: now, Rows: len(rows)})
		json.NewEncoder(w).Encode(rows)
	case "allMids":
		v.mu.Lock()
		status := v.mids
		v.mu.Unlock()
		v.record(rdRequest{Type: body.Type, At: now})
		if status != http.StatusOK {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(status)
			return
		}
		out := map[string]string{}
		for _, c := range rdCoins {
			out[c] = "101"
		}
		json.NewEncoder(w).Encode(out)
	case "fundingHistory":
		coin := body.Coin
		v.mu.Lock()
		lag, conflict, drop, fail := v.fundingLag, v.fundingConflict[coin], v.fundingDrop[coin], v.fundingFail[coin]
		v.mu.Unlock()
		if fail {
			v.record(rdRequest{Type: body.Type, Coin: coin, Start: body.StartTime, End: body.EndTime, At: now})
			http.Error(w, "venue failure", http.StatusInternalServerError)
			return
		}
		published := now.Add(-lag).UnixMilli()
		rows := []map[string]any{}
		for h := (body.StartTime + hlFundingRecordStepMs - 1) / hlFundingRecordStepMs * hlFundingRecordStepMs; h <= body.EndTime && h <= published && len(rows) < hlFundingRowCap; h += hlFundingRecordStepMs {
			if h == drop {
				continue
			}
			rate := rdFundingRate(coin, h)
			if h == conflict {
				rate += 0.5
			}
			rows = append(rows, map[string]any{"coin": coin, "fundingRate": strconv.FormatFloat(rate, 'f', -1, 64), "premium": "0", "time": h})
		}
		v.record(rdRequest{Type: body.Type, Coin: coin, Start: body.StartTime, End: body.EndTime, At: now, Rows: len(rows)})
		json.NewEncoder(w).Encode(rows)
	default:
		w.Write([]byte(`[]`))
	}
}

type rdFixture struct {
	t      *testing.T
	ctx    context.Context
	clock  *hlLotTestClock
	venue  *rdVenue
	ledger *feedRequestLedger
	owner  *marketFeedOwner
	sealer *feedSealer
	union  feedRequirements
	mu     sync.Mutex
	logs   []string
}

func rdStrategy(id, name, coin, tf string, interval int, closed, htf, drawdown bool) StrategyConfig {
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Type: "perps", Script: hyperliquidCheckScript,
		Args: []string{name, coin, tf, "--mode=paper"}, IntervalSeconds: interval, ClosedBarDecisions: closed, HTFFilter: htf, Capital: 1000}
	if drawdown {
		sc.MaxDrawdownPct = 20
	}
	return sc
}

func rdConsumer(t *testing.T, path string, strategies []StrategyConfig) feedConsumer {
	t.Helper()
	cfg := &Config{IntervalSeconds: 300, Strategies: strategies}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive %s: %v", path, err)
	}
	return feedConsumer{Path: path, Loaded: true, Req: req, Cadences: feedConsumerCadences(cfg), Strategies: len(req.Strategies)}
}

func rdWorkload(t *testing.T, formingFourHour bool) []feedConsumer {
	t.Helper()
	var live, paper []StrategyConfig
	for _, coin := range rdCoins {
		live = append(live,
			rdStrategy("live-"+coin+"-5m", "sma_crossover", coin, "5m", 300, false, false, true),
			rdStrategy("live-"+coin+"-15m", "rsi", coin, "15m", 300, false, false, true))
		paper = append(paper,
			rdStrategy("paper-"+coin+"-30m", "funding_skew", coin, "30m", 300, false, false, false),
			rdStrategy("paper-"+coin+"-1h", "sma_crossover", coin, "1h", 300, true, true, false))
		if formingFourHour {
			paper = append(paper, rdStrategy("paper-"+coin+"-30m-htf", "rsi", coin, "30m", 300, false, true, false))
		}
	}
	return []feedConsumer{rdConsumer(t, "/live.json", live), rdConsumer(t, "/paper.json", paper)}
}

func newRDFixture(t *testing.T, start time.Time, build func() []feedConsumer) (*rdFixture, []feedConsumer) {
	t.Helper()
	f := &rdFixture{t: t, clock: &hlLotTestClock{now: start}}
	f.venue = newRDVenue(t, f.clock)
	useHLInfoURLs(t, f.venue.srv.URL, f.venue.srv.URL+"/alt-host")
	f.ledger = newFeedRequestLedger(nil, false, f.clock.Now)
	f.ctx = withFeedLedger(context.Background(), f.ledger)
	f.owner = newMarketFeedOwner(f.clock.Now, f.logf)
	f.owner.historyJitter = func(time.Duration) time.Duration { return 0 }
	s := newFeedSealer(f.owner, feedSourceREST, "inst-rest", f.clock.Now, f.logf)
	s.ledger = f.ledger
	src := newFeedRESTSource(f.owner, f.logf)
	src.backoff = time.Millisecond
	s.prepareFn = src.prepare
	f.sealer = s
	consumers := build()
	f.install(unionFeedRequirements(consumers), unionFeedCadences(consumers))
	return f, consumers
}

func (f *rdFixture) logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *rdFixture) logsContaining(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.logs {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func (f *rdFixture) install(union feedRequirements, cadences []int) {
	f.t.Helper()
	f.union = union
	select {
	case <-f.owner.ApplyGeneration(f.ctx, union):
	case <-time.After(30 * time.Second):
		f.t.Fatal("feed generation did not publish")
	}
	f.sealer.setGeneration(union, cadences, f.clock.Now().Add(-time.Hour))
}

func (f *rdFixture) seal(deadline int64) (*feedSealDoc, []rdRequest) {
	f.t.Helper()
	mark := f.venue.mark()
	f.clock.mu.Lock()
	f.clock.now = time.Unix(deadline, 0).UTC().Add(feedSealSettleDelay)
	f.clock.mu.Unlock()
	f.sealer.sealOne(f.ctx, deadline)
	f.sealer.mu.Lock()
	seal := f.sealer.ring[deadline]
	f.sealer.mu.Unlock()
	if seal == nil {
		f.t.Fatalf("deadline %d stored no seal; logs: %v", deadline, f.logsContaining("[feed-seal]"))
	}
	doc, err := decodeFeedSeal(seal.Bytes, deadline)
	if err != nil {
		f.t.Fatal(err)
	}
	return doc, f.venue.since(mark, "")
}

func rdSealKey(t *testing.T, doc *feedSealDoc, key marketFeedKey) feedSealKey {
	t.Helper()
	for _, k := range doc.Keys {
		if k.Symbol == key.Symbol && k.Timeframe == key.Timeframe {
			return k
		}
	}
	t.Fatalf("seal %d has no key %s", doc.Key, key)
	return feedSealKey{}
}

func rdCandleKeys(reqs []rdRequest) map[string]int {
	out := map[string]int{}
	for _, r := range reqs {
		if r.Type == "candleSnapshot" {
			out[r.Coin+"|"+r.Interval]++
		}
	}
	return out
}

func rdTimeframeCount(reqs []rdRequest, tf string) int {
	n := 0
	for _, r := range reqs {
		if r.Type == "candleSnapshot" && r.Interval == tf {
			n++
		}
	}
	return n
}

func rdBase() int64 {
	const fourHours = int64(14_400)
	return (int64(1_890_000_000)/fourHours + 1) * fourHours
}

func TestFeedRESTDemandEligibilityAndReuse(t *testing.T) {
	t0 := rdBase()
	f, consumers := newRDFixture(t, time.Unix(t0-600, 0).UTC(), func() []feedConsumer { return rdWorkload(t, false) })
	union := f.union
	if len(union.Order) != 25 {
		t.Fatalf("union keys = %d, want 25 (five coins at 5m, 15m, 30m, 1h and 4h)", len(union.Order))
	}
	fourH, oneH, fiveM := feedKeyFor("BTC", "4h"), feedKeyFor("BTC", "1h"), feedKeyFor("BTC", "5m")
	if !union.closedOnlyReuse(fourH) || union.closedOnlyReuse(oneH) || union.closedOnlyReuse(fiveM) {
		t.Fatalf("closed-only keys = %v, want only the 4h keys every reader reads as a closed-bar higher timeframe", union.ClosedOnlyKeys)
	}
	if got := formatCadences(union.KeyCadences[fourH]); got != "300s" {
		t.Fatalf("4h cadences = %s, want 300s: only the 300s consumer reads it", got)
	}
	if got := formatCadences(union.KeyCadences[fiveM]); got != "90s,300s" {
		t.Fatalf("5m cadences = %s, want 90s,300s: the drawdown acceleration reaches 90s", got)
	}

	doc, reqs := f.seal(t0)
	if got := rdCandleKeys(reqs); len(got) != 25 {
		t.Fatalf("coincident deadline read %d keys (%v), want each of the 25 keys once", len(got), got)
	}
	for k, n := range rdCandleKeys(reqs) {
		if n != 1 {
			t.Fatalf("coincident deadline read %s %d times, want once", k, n)
		}
	}
	if r := rdSealKey(t, doc, fourH).Readiness; !r.Ready || strings.Contains(r.Detail, "reused") {
		t.Fatalf("4h at its own close = %+v, want a fresh ready read", r)
	}

	doc, reqs = f.seal(t0 + 90)
	if n := len(rdCandleKeys(reqs)); n != 10 || rdTimeframeCount(reqs, "4h") != 0 || rdTimeframeCount(reqs, "1h") != 0 {
		t.Fatalf("90s-only deadline read %v, want only the ten 5m and 15m keys the 90s cadence can reach", rdCandleKeys(reqs))
	}
	if r := rdSealKey(t, doc, fourH).Readiness; r.Ready || r.Status != string(feedStatusNotDue) {
		t.Fatalf("4h at a 90s-only deadline = %+v, want not_due and not ready", r)
	}

	doc, reqs = f.seal(t0 + 300)
	if rdTimeframeCount(reqs, "4h") != 0 || rdTimeframeCount(reqs, "1h") != 5 || rdTimeframeCount(reqs, "5m") != 5 {
		t.Fatalf("300s deadline read %v, want every key except the reused 4h keys", rdCandleKeys(reqs))
	}
	sk := rdSealKey(t, doc, fourH)
	if !sk.Readiness.Ready || !strings.Contains(sk.Readiness.Detail, "reused for deadline") {
		t.Fatalf("reused 4h = %+v, want ready with the reuse detail", sk.Readiness)
	}
	last := sk.Bars[len(sk.Bars)-1]
	if last.CloseMs != t0*1000-1 || last.Close != rdBarClose("BTC", last.OpenMs, 14_400_000) {
		t.Fatalf("reused 4h newest bar = %+v, want the final values of the bar closed at %d and no forming bar", last, t0*1000-1)
	}
	if sk.Readiness.LastCloseMs != last.CloseMs || sk.Readiness.Bars != len(sk.Bars) {
		t.Fatalf("reused readiness = %+v, want it to describe the omitted forming bar", sk.Readiness)
	}
	if got := f.logsContaining("[feed-reuse] key=" + strconv.FormatInt(t0+300, 10)); len(got) != 1 || !strings.Contains(got[0], "reused=5 ") {
		t.Fatalf("reuse log = %q, want five reused keys", got)
	}

	f.venue.mu.Lock()
	f.venue.fail["BTC|4h"] = true
	f.venue.mu.Unlock()
	if err := f.owner.fetchAndMerge(f.ctx, fourH, feedRestRecovery); err == nil {
		t.Fatal("the failing venue read succeeded")
	}
	doc, reqs = f.seal(t0 + 600)
	if rdCandleKeys(reqs)["BTC|4h"] == 0 || rdTimeframeCount(reqs, "4h") != rdCandleKeys(reqs)["BTC|4h"] {
		t.Fatalf("after a failed read the 4h reads were %v, want BTC 4h read again and the others reused", rdCandleKeys(reqs))
	}
	if r := rdSealKey(t, doc, fourH).Readiness; r.Ready {
		t.Fatalf("BTC 4h after a failed read and failed refresh = %+v, want not ready", r)
	}
	if r := rdSealKey(t, doc, feedKeyFor("ETH", "4h")).Readiness; !r.Ready || !strings.Contains(r.Detail, "reused") {
		t.Fatalf("ETH 4h = %+v, want reused", r)
	}
	f.venue.mu.Lock()
	f.venue.fail["BTC|4h"] = false
	f.venue.mu.Unlock()
	doc, reqs = f.seal(t0 + 900)
	if rdCandleKeys(reqs)["BTC|4h"] != 1 {
		t.Fatalf("recovery deadline reads = %v, want BTC 4h read", rdCandleKeys(reqs))
	}
	if r := rdSealKey(t, doc, fourH).Readiness; !r.Ready || strings.Contains(r.Detail, "reused") {
		t.Fatalf("BTC 4h after recovery = %+v, want a fresh ready read", r)
	}
	_, reqs = f.seal(t0 + 1200)
	if rdTimeframeCount(reqs, "4h") != 0 {
		t.Fatalf("after recovery the 4h reads were %v, want reuse again", rdCandleKeys(reqs))
	}

	f.venue.mu.Lock()
	f.venue.mids = http.StatusTooManyRequests
	f.venue.mu.Unlock()
	doc, reqs = f.seal(t0 + 1500)
	if rdTimeframeCount(reqs, "4h") != 0 {
		t.Fatalf("pause deadline 4h reads = %v, want none before the pause", rdCandleKeys(reqs))
	}
	if r := rdSealKey(t, doc, fourH).Readiness; r.Ready || r.Status != string(feedStatusRateLimited) || !strings.Contains(r.Detail, "paused") {
		t.Fatalf("4h when a later read in the preparation raised a host pause = %+v, want rate_limited and not ready", r)
	}
	f.venue.mu.Lock()
	f.venue.mids = http.StatusOK
	f.venue.mu.Unlock()
	f.clock.Advance(2 * time.Minute)
	doc, reqs = f.seal(t0 + 1800)
	if rdTimeframeCount(reqs, "4h") != 5 {
		t.Fatalf("after the pause the 4h reads were %v, want each 4h key read before reuse", rdCandleKeys(reqs))
	}
	if r := rdSealKey(t, doc, fourH).Readiness; !r.Ready {
		t.Fatalf("4h after the pause = %+v, want ready", r)
	}

	raised := f.union
	raised.Keys = map[marketFeedKey]int{}
	for k, v := range f.union.Keys {
		raised.Keys[k] = v
	}
	raised.Keys[fourH] += 20
	mark := f.venue.mark()
	f.install(raised, unionFeedCadences(consumers))
	if n := len(f.venue.since(mark, "candleSnapshot")); n == 0 {
		t.Fatal("a raised lookback sent no history read")
	}
	doc, _ = f.seal(t0 + 2100)
	if r := rdSealKey(t, doc, fourH).Readiness; !r.Ready || r.Required < raised.Keys[fourH] {
		t.Fatalf("4h after the raised lookback = %+v, want ready on the full %d-bar history", r, raised.Keys[fourH])
	}

	nextClose := t0 + 14_400
	doc, reqs = f.seal(nextClose)
	if rdTimeframeCount(reqs, "4h") != 5 {
		t.Fatalf("a new 4h close read %v, want every 4h key read", rdCandleKeys(reqs))
	}
	sk = rdSealKey(t, doc, fourH)
	if sk.Readiness.Detail != "" && strings.Contains(sk.Readiness.Detail, "reused") {
		t.Fatalf("4h at a new close = %+v, want a fresh read", sk.Readiness)
	}
	doc, _ = f.seal(nextClose + 300)
	sk = rdSealKey(t, doc, fourH)
	last = sk.Bars[len(sk.Bars)-1]
	if !strings.Contains(sk.Readiness.Detail, "reused") || last.CloseMs != nextClose*1000-1 || last.Close != rdBarClose("BTC", last.OpenMs, 14_400_000) {
		t.Fatalf("after the new close the reused 4h ends at %+v (%s), want the final bar closed at %d", last, sk.Readiness.Detail, nextClose*1000-1)
	}
}

func TestFeedRESTReuseNeedsEveryReaderClosed(t *testing.T) {
	t0 := rdBase()
	t.Run("FormingReader", func(t *testing.T) {
		f, _ := newRDFixture(t, time.Unix(t0-600, 0).UTC(), func() []feedConsumer { return rdWorkload(t, true) })
		if f.union.closedOnlyReuse(feedKeyFor("BTC", "4h")) {
			t.Fatal("a 4h key that a non-closed higher-timeframe filter reads must not be reused")
		}
		f.seal(t0)
		_, reqs := f.seal(t0 + 300)
		if rdTimeframeCount(reqs, "4h") != 5 {
			t.Fatalf("4h reads = %v, want every 4h key read at each due deadline", rdCandleKeys(reqs))
		}
	})
	t.Run("UnloadedConsumer", func(t *testing.T) {
		consumers := append(rdWorkload(t, false), feedConsumer{Path: "/unreadable.json", Err: "permission denied"})
		union := unionFeedRequirements(consumers)
		if union.closedOnlyReuse(feedKeyFor("BTC", "4h")) || !strings.Contains(union.ReuseBlocked, "/unreadable.json") {
			t.Fatalf("reuse with an unreadable consumer = %q, want blocked because its readers are unknown", union.ReuseBlocked)
		}
		retained := rdWorkload(t, false)
		retained[1].Retained = true
		if !unionFeedRequirements(retained).closedOnlyReuse(feedKeyFor("BTC", "4h")) {
			t.Fatal("a retained consumer keeps its previous readers, so reuse stays proved")
		}
	})
	t.Run("RegimeBundleReader", func(t *testing.T) {
		cfg := &Config{IntervalSeconds: 300, Strategies: []StrategyConfig{rdStrategy("c", "sma_crossover", "BTC", "1h", 300, true, true, false)}}
		req, err := deriveFeedRequirements(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !req.ClosedKeys[feedKeyFor("BTC", "4h")] || req.FormingKeys[feedKeyFor("BTC", "4h")] {
			t.Fatalf("closed-bar HTF readers = %v forming = %v", req.ClosedKeys, req.FormingKeys)
		}
		cfg.Regime = &RegimeConfig{Enabled: true, Timeframe: "4h"}
		if req, err = deriveFeedRequirements(cfg); err != nil {
			t.Skipf("regime config not derivable in this fixture: %v", err)
		}
		if !req.FormingKeys[feedKeyFor("BTC", "4h")] {
			t.Fatalf("a regime bundle on the 4h key reads the forming bar; forming = %v", req.FormingKeys)
		}
	})
}

func TestFeedRESTDemandEstimateMatchesMeasured(t *testing.T) {
	t0 := rdBase()
	f, _ := newRDFixture(t, time.Unix(t0-600, 0).UTC(), func() []feedConsumer { return rdWorkload(t, false) })
	lines := estimateFeedDemandLines(f.union, feedSourceREST)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"class=90s ", "class=300s ", "class=90s+300s ", "rolling_60s_peak", "weight demand funding:", "assumptions"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("estimate lines lack %q:\n%s", want, joined)
		}
	}
	var estPeak int
	for _, l := range lines {
		if i := strings.Index(l, "candles_and_mids="); i >= 0 {
			fmt.Sscanf(l[i:], "candles_and_mids=%d", &estPeak)
		}
	}
	type charge struct {
		at time.Time
		w  int
	}
	var charges []charge
	var deadlines []int64
	for d := t0; d <= t0+2700; d++ {
		if d%90 == 0 || d%300 == 0 {
			deadlines = append(deadlines, d)
		}
	}
	for _, d := range deadlines {
		_, reqs := f.seal(d)
		for _, r := range reqs {
			switch r.Type {
			case "candleSnapshot":
				w, _ := hlInfoWeight("candleSnapshot", r.Rows)
				charges = append(charges, charge{at: time.Unix(d, 0), w: w})
			case "allMids":
				w, _ := hlInfoWeight("allMids", 0)
				charges = append(charges, charge{at: time.Unix(d, 0), w: w})
			}
		}
	}
	measured := 0
	for i := range charges {
		sum := 0
		for j := range charges {
			if !charges[j].at.After(charges[i].at) && charges[i].at.Sub(charges[j].at) < time.Minute {
				sum += charges[j].w
			}
		}
		measured = max(measured, sum)
	}
	if measured == 0 || estPeak < measured {
		t.Fatalf("measured rolling candle and mids peak %d over three 900s cycles is above the estimate %d:\n%s", measured, estPeak, joined)
	}
	t.Logf("estimate:\n%s\nmeasured rolling 60s candles+mids peak=%d", joined, measured)
}

func TestFeedFundingRecordTail(t *testing.T) {
	t0 := rdBase()
	clock := &hlLotTestClock{now: time.Unix(t0+600, 0).UTC()}
	venue := newRDVenue(t, clock)
	useHLInfoURLs(t, venue.srv.URL, venue.srv.URL+"/alt-host")
	ledger := newFeedRequestLedger(nil, false, clock.Now)
	ctx := withFeedLedger(context.Background(), ledger)
	var logs []string
	var logMu sync.Mutex
	owner := newMarketFeedOwner(clock.Now, func(f string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	owner.feedMu.Lock()
	owner.fundingNeeds = map[string]feedFundingNeed{}
	for _, c := range rdCoins {
		owner.fundingNeeds[c] = feedFundingNeed{Records: true}
	}
	owner.feedMu.Unlock()
	frameStart := map[string]int64{}
	for _, c := range rdCoins {
		frameStart[c] = (t0 - 40*86_400) * 1000
	}
	truth := func(coin string, startMs int64) []feedFundingRecord {
		var out []feedFundingRecord
		published := clock.Now().Add(-venue.fundingLag).UnixMilli()
		for h := (startMs + hlFundingRecordStepMs - 1) / hlFundingRecordStepMs * hlFundingRecordStepMs; h <= clock.Now().UnixMilli() && h <= published; h += hlFundingRecordStepMs {
			rate := rdFundingRate(coin, h)
			if h == venue.fundingConflict[coin] {
				rate += 0.5
			}
			out = append(out, feedFundingRecord{TimeMs: h, Rate: rate})
		}
		return out
	}
	check := func(stage string) {
		t.Helper()
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		for _, c := range rdCoins {
			f := owner.funding[c]
			want := truth(c, frameStart[c])
			if f == nil || f.Err != "" || !f.HasRecords || len(f.Records) != len(want) {
				t.Fatalf("%s: %s funding = %+v, want %d records", stage, c, f, len(want))
			}
			for i := range want {
				if f.Records[i] != want[i] {
					t.Fatalf("%s: %s record %d = %+v, want %+v", stage, c, i, f.Records[i], want[i])
				}
			}
		}
	}
	pagesByCoin := func(reqs []rdRequest) map[string][]rdRequest {
		out := map[string][]rdRequest{}
		for _, r := range reqs {
			if r.Type == "fundingHistory" {
				out[r.Coin] = append(out[r.Coin], r)
			}
		}
		return out
	}

	mark := venue.mark()
	owner.EnsureFunding(ctx, frameStart)
	check("first load")
	for c, pages := range pagesByCoin(venue.since(mark, "")) {
		if len(pages) != 2 || pages[0].Start != frameStart[c] || pages[0].Rows != hlFundingRowCap || pages[1].Rows >= hlFundingRowCap {
			t.Fatalf("%s first load pages = %+v, want two pages over the 960-hour range with no terminal probe", c, pages)
		}
	}

	clock.Advance(feedFundingTTL + time.Minute)
	for c := range frameStart {
		frameStart[c] += 2 * hlFundingRecordStepMs
	}
	mark = venue.mark()
	owner.EnsureFunding(ctx, frameStart)
	check("tail")
	for c, pages := range pagesByCoin(venue.since(mark, "")) {
		owner.feedMu.Lock()
		f := owner.funding[c]
		owner.feedMu.Unlock()
		if len(pages) != 1 || pages[0].End != clock.Now().UnixMilli() || pages[0].Start > f.Records[len(f.Records)-1].TimeMs {
			t.Fatalf("%s tail pages = %+v, want one bounded page from the anchor with the inclusive end time", c, pages)
		}
	}

	clock.Advance(feedFundingTTL + time.Minute)
	venue.mu.Lock()
	venue.fundingLag = 2 * time.Hour
	venue.mu.Unlock()
	mark = venue.mark()
	owner.EnsureFunding(ctx, frameStart)
	for c, pages := range pagesByCoin(venue.since(mark, "")) {
		if len(pages) < 2 || pages[0].Rows != 0 {
			t.Fatalf("%s tail with the anchor no longer published = %+v, want an empty tail then a full read", c, pages)
		}
	}
	check("missing anchor")
	venue.mu.Lock()
	venue.fundingLag = 0
	venue.mu.Unlock()

	clock.Advance(feedFundingTTL + time.Minute)
	owner.feedMu.Lock()
	anchor := owner.funding["BTC"].Records[len(owner.funding["BTC"].Records)-1].TimeMs
	owner.feedMu.Unlock()
	venue.mu.Lock()
	venue.fundingConflict["BTC"] = anchor
	venue.mu.Unlock()
	mark = venue.mark()
	owner.EnsureFunding(ctx, frameStart)
	if pages := pagesByCoin(venue.since(mark, ""))["BTC"]; len(pages) < 2 {
		t.Fatalf("BTC conflict pages = %+v, want the refused tail followed by a full read", pages)
	}
	owner.feedMu.Lock()
	got := owner.funding["BTC"]
	owner.feedMu.Unlock()
	if got.Err != "" || len(got.Records) == 0 {
		t.Fatalf("BTC after the conflict = %+v, want the full read accepted", got)
	}
	check("conflict")
	clock.Advance(feedFundingTTL + time.Minute)
	owner.EnsureFunding(ctx, frameStart)
	check("after conflict")

	clock.Advance(feedFundingTTL + time.Minute)
	venue.mu.Lock()
	venue.fundingFail["ETH"] = true
	venue.mu.Unlock()
	owner.EnsureFunding(ctx, frameStart)
	snap := freezeMarketSnapshot(owner, cycleMarketRequirements{Funding: map[string]feedFundingNeed{"ETH": {Records: true}}}, "x", clock.Now(), clock.Now())
	if held, why := snap.fundingHold("ETH", false, true); !held {
		t.Fatal("a failed funding read must hold readers")
	} else if !strings.Contains(why, "failed") {
		t.Fatalf("hold reason = %q", why)
	}
	venue.mu.Lock()
	venue.fundingFail["ETH"] = false
	venue.mu.Unlock()
	clock.Advance(time.Second)
	mark = venue.mark()
	owner.EnsureFunding(ctx, frameStart)
	if pages := pagesByCoin(venue.since(mark, ""))["ETH"]; len(pages) == 0 || pages[0].Start != frameStart["ETH"] {
		t.Fatalf("ETH recovery pages = %+v, want a full read from the frame start", pages)
	}
	check("recovery")

	wider := map[string]int64{}
	for c, v := range frameStart {
		wider[c] = v
	}
	wider["SOL"] -= 10 * hlFundingRecordStepMs
	clock.Advance(time.Second)
	mark = venue.mark()
	owner.EnsureFunding(ctx, wider)
	pages := pagesByCoin(venue.since(mark, ""))
	if len(pages["SOL"]) == 0 || pages["SOL"][0].Start != wider["SOL"] {
		t.Fatalf("a wider SOL frame before expiry read %+v, want a full read from the earlier start", pages["SOL"])
	}
	if len(pages["BTC"]) != 0 {
		t.Fatalf("an unchanged BTC frame before expiry read %+v, want the cache", pages["BTC"])
	}
	frameStart = wider
	check("wider frame")

	logMu.Lock()
	defer logMu.Unlock()
	var modes []string
	for _, l := range logs {
		if strings.HasPrefix(l, "[feed-funding]") {
			modes = append(modes, l)
		}
	}
	sort.Strings(modes)
	if len(modes) == 0 {
		t.Fatal("no [feed-funding] lines were logged")
	}
}

func TestFeedRESTReuseConsumerReaderMismatch(t *testing.T) {
	t0 := rdBase()
	f, consumers := newRDFixture(t, time.Unix(t0-600, 0).UTC(), func() []feedConsumer {
		consumers := rdWorkload(t, false)
		consumers[1].Req.Keys[feedKeyFor("BTC", "4h")] = hlBatchPythonDefaultOhlcvLimit
		return consumers
	})
	fresh, _ := f.seal(t0)
	reused, _ := f.seal(t0 + 300)
	fourH := feedKeyFor("BTC", "4h")
	if reused.V != feedSealVersionClosedReuse || !rdSealKey(t, reused, fourH).FormingBarOmitted {
		t.Fatal("reused 4h frame did not carry the versioned forming-bar omission")
	}

	formingHTF := rdStrategy("forming-htf", "rsi", "BTC", "30m", 300, false, true, false)
	formingSignal := rdStrategy("forming-signal", "sma_crossover", "BTC", "4h", 300, false, false, false)
	closedHTF := rdStrategy("closed-htf", "sma_crossover", "BTC", "1h", 300, true, true, false)

	check := func(t *testing.T, doc *feedSealDoc, sc StrategyConfig, held, formingIncluded bool) {
		t.Helper()
		req := rdConsumer(t, "/paper.json", []StrategyConfig{sc}).Req
		ctx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: doc.snapshot()}
		hold := ctx.holdFor(sc)
		raw, err := ctx.singleCheckPayload(sc)
		if held {
			if !hold.Held || hold.Key != fourH || !strings.Contains(hold.Reason, "without the forming bar") ||
				err == nil || !strings.Contains(err.Error(), "without the forming bar") || raw != nil {
				t.Fatalf("hold = %+v, payload error = %v, want no evaluation of the reused 4h frame", hold, err)
			}
			return
		}
		if hold.Held || err != nil {
			t.Fatalf("hold = %+v, payload error = %v, want a usable frame", hold, err)
		}
		var envelope marketStdinEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if frame, ok := envelope.Market.Frames[fourH.PayloadID()]; !ok || frame.FormingBarIncluded != formingIncluded {
			t.Fatalf("4h present = %t, forming bar included = %t, want true and %t", ok, frame.FormingBarIncluded, formingIncluded)
		}
	}

	t.Run("NewFormingHTF", func(t *testing.T) {
		check(t, reused, formingHTF, true, false)
	})
	t.Run("NewSignalWithRetainedFeedConfig", func(t *testing.T) {
		consumers[1].Retained = true
		f.install(unionFeedRequirements(consumers), unionFeedCadences(consumers))
		retained, _ := f.seal(t0 + 600)
		check(t, retained, formingSignal, true, false)
	})
	t.Run("ClosedReuseAndFreshForming", func(t *testing.T) {
		check(t, reused, closedHTF, false, false)
		check(t, fresh, formingHTF, false, true)
	})
}
