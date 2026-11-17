package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const fhStep = int64(60_000)

var fhBase = time.Unix(1890000000, 0).UTC()

type fhRequest struct {
	Start int64
	End   int64
}

type fhHold struct {
	entered chan struct{}
	gate    chan struct{}
}

type fhVenue struct {
	srv   *httptest.Server
	mu    sync.Mutex
	mode  map[string]string
	from  map[string]int64
	reqs  map[string][]fhRequest
	holds map[string][]*fhHold
}

func newFHVenue(t *testing.T) *fhVenue {
	t.Helper()
	v := &fhVenue{mode: map[string]string{}, from: map[string]int64{}, reqs: map[string][]fhRequest{}, holds: map[string][]*fhHold{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fhVenue) set(coin, mode string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mode[coin] = mode
}

func (v *fhVenue) setBoundary(coin string, fromMs int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.mode[coin] = "boundary"
	v.from[coin] = fromMs
}

func (v *fhVenue) hold(coin string) (<-chan struct{}, func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	h := &fhHold{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	v.holds[coin] = append(v.holds[coin], h)
	var once sync.Once
	return h.entered, func() { once.Do(func() { close(h.gate) }) }
}

func (v *fhVenue) requests(coin string) []fhRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]fhRequest(nil), v.reqs[coin]...)
}

func (v *fhVenue) count(coin string) int {
	return len(v.requests(coin))
}

func (v *fhVenue) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
		Req  struct {
			Coin      string `json:"coin"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		} `json:"req"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	w.Header().Set("Content-Type", "application/json")
	if body.Type != "candleSnapshot" {
		w.Write([]byte(`[]`))
		return
	}
	coin := body.Req.Coin
	v.mu.Lock()
	v.reqs[coin] = append(v.reqs[coin], fhRequest{Start: body.Req.StartTime, End: body.Req.EndTime})
	n := len(v.reqs[coin])
	var h *fhHold
	if q := v.holds[coin]; len(q) > 0 {
		h = q[0]
		v.holds[coin] = q[1:]
	}
	v.mu.Unlock()
	if h != nil {
		h.entered <- struct{}{}
		<-h.gate
	}
	v.mu.Lock()
	mode, from := v.mode[coin], v.from[coin]
	v.mu.Unlock()
	switch mode {
	case "500":
		http.Error(w, "venue failure", http.StatusInternalServerError)
		return
	case "429":
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case "empty":
		w.Write([]byte(`[]`))
		return
	}
	row := func(open int64) map[string]any {
		return map[string]any{"t": open, "T": open + fhStep - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"}
	}
	first := (body.Req.StartTime + fhStep - 1) / fhStep * fhStep
	rows := []map[string]any{}
	for open := first; open <= body.Req.EndTime && len(rows) < hlCandleMaxCandles; open += fhStep {
		if mode == "boundary" && open < from {
			continue
		}
		rows = append(rows, row(open))
	}
	if mode == "dup" {
		var dup []map[string]any
		for _, r := range rows[len(rows)/2+1:] {
			dup = append(dup, r, r)
		}
		rows = dup
	}
	if mode == "grow" && len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	if mode == "invalid" && len(rows) > 0 {
		rows[len(rows)-1]["h"] = "50"
	}
	json.NewEncoder(w).Encode(rows)
}

type fhEnv struct {
	t      *testing.T
	clock  *hlLotTestClock
	venue  *fhVenue
	ledger *feedRequestLedger
	ctx    context.Context
	owner  *marketFeedOwner
	mu     sync.Mutex
	logs   []string
}

func newFHEnv(t *testing.T, start time.Time) *fhEnv {
	t.Helper()
	e := &fhEnv{t: t, clock: &hlLotTestClock{now: start}}
	e.venue = newFHVenue(t)
	useHLInfoURLs(t, e.venue.srv.URL, e.venue.srv.URL)
	e.ledger = newFeedRequestLedger(nil, false, e.clock.Now)
	e.ctx = withFeedLedger(context.Background(), e.ledger)
	e.owner = e.newOwner()
	return e
}

func (e *fhEnv) newOwner() *marketFeedOwner {
	o := newMarketFeedOwner(e.clock.Now, e.logf)
	o.SetRequestLedger(e.ledger)
	o.historyJitter = func(time.Duration) time.Duration { return 0 }
	return o
}

func (e *fhEnv) logf(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
}

func (e *fhEnv) logsContaining(sub string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, l := range e.logs {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func (e *fhEnv) apply(o *marketFeedOwner, req feedRequirements) {
	e.t.Helper()
	select {
	case <-o.ApplyGeneration(e.ctx, req):
	case <-time.After(10 * time.Second):
		e.t.Fatal("feed generation did not publish")
	}
}

func (e *fhEnv) state(o *marketFeedOwner, key marketFeedKey) feedKeyState {
	e.t.Helper()
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	st := o.keys[key]
	if st == nil {
		e.t.Fatalf("key %s is not tracked", key)
	}
	cp := *st
	cp.Bars = append([]feedBar(nil), st.Bars...)
	return cp
}

func (e *fhEnv) ready(o *marketFeedOwner, key marketFeedKey) feedKeyReadiness {
	e.t.Helper()
	r, ok := o.readinessFor(key)
	if !ok {
		e.t.Fatalf("key %s is not tracked", key)
	}
	return r
}

func (e *fhEnv) socketBars(o *marketFeedOwner, key marketFeedKey, n int) {
	lastOpen := e.clock.Now().UnixMilli() / fhStep * fhStep
	for i := n - 1; i >= 0; i-- {
		open := lastOpen - int64(i)*fhStep
		o.IngestSocketCandle(key, hlCandleRaw{OpenMs: open, CloseMs: open + fhStep - 1, HasClose: i != 0,
			Open: 100, High: 102, Low: 99, Close: 101, Volume: 5}, e.clock.Now())
	}
}

func (e *fhEnv) fullWindow(req fhRequest, lookback int) bool {
	return req.End-req.Start >= int64(lookback)*fhStep
}

func fhWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func runFeedHistoryPass(t *testing.T, o *marketFeedOwner, ctx context.Context) []error {
	t.Helper()
	var errs []error
	for _, job := range o.historyPass(feedHistoryWorkers) {
		if err := o.runHistoryJob(ctx, job); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func TestFeedHistoryCompletion(t *testing.T) {
	t.Run("FailedStartupNeedsFullHistory", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		e.venue.set("BTC", "500")
		e.venue.set("ETH", "500")
		req := rlRequirements(100, 60, "BTC", "ETH", "SOL")
		btc, eth, sol := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m"), feedKeyFor("SOL", "1m")
		e.apply(e.owner, req)
		e.socketBars(e.owner, btc, 1)
		e.socketBars(e.owner, eth, 40)
		for key, bars := range map[marketFeedKey]int{btc: 1, eth: 40} {
			r := e.ready(e.owner, key)
			if r.Ready || r.Bars != bars || r.HistoryLookback != 0 {
				t.Fatalf("%s after a failed start with %d socket bars = %+v, want not ready with no covered history", key, bars, r)
			}
		}
		if r := e.ready(e.owner, sol); !r.Ready || r.HistoryLookback != 100 {
			t.Fatalf("SOL = %+v, want ready with the 100-bar lookback covered", r)
		}
		before := e.venue.count("BTC") + e.venue.count("ETH")
		prepareMarketSnapshot(e.ctx, e.owner, fullCycleRequirements(req))
		if got := e.venue.count("BTC") + e.venue.count("ETH"); got != before {
			t.Fatalf("seal preparation sent %d request(s) for keys without history, want none", got-before)
		}
		e.venue.set("BTC", "ok")
		e.venue.set("ETH", "ok")
		e.clock.Advance(feedHistoryRetryBase)
		if errs := runFeedHistoryPass(t, e.owner, e.ctx); len(errs) != 0 {
			t.Fatalf("history pass errors: %v", errs)
		}
		for _, coin := range []string{"BTC", "ETH"} {
			reqs := e.venue.requests(coin)
			last := reqs[len(reqs)-1]
			if len(reqs) != 2 || !e.fullWindow(last, 100) {
				t.Fatalf("%s requests = %+v, want the next attempt to ask for the full 100-bar window", coin, reqs)
			}
			if r := e.ready(e.owner, feedKeyFor(coin, "1m")); !r.Ready || r.HistoryLookback != 100 {
				t.Fatalf("%s after the history load = %+v, want ready", coin, r)
			}
		}
		solBars := e.state(e.owner, sol).Bars
		if err := e.owner.fetchAndMerge(e.ctx, sol, feedRestRefresh); err != nil {
			t.Fatal(err)
		}
		reqs := e.venue.requests("SOL")
		last := reqs[len(reqs)-1]
		if want := solBars[len(solBars)-1].OpenMs - fhStep; last.Start != want {
			t.Fatalf("complete key refresh start = %d, want the short window from %d", last.Start, want)
		}
		if got := e.logsContaining("status=recovered"); len(got) != 2 {
			t.Fatalf("recovered lines = %q, want one per recovered key", got)
		}
	})

	t.Run("EmptyInvalidDuplicateNeverComplete", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		e.venue.set("AAA", "empty")
		e.venue.set("BBB", "invalid")
		e.venue.set("CCC", "dup")
		e.venue.set("DDD", "empty")
		entered, release := e.venue.hold("DDD")
		done := e.owner.ApplyGeneration(e.ctx, rlRequirements(100, 60, "AAA", "BBB", "CCC", "DDD"))
		fhWait(t, entered, "the DDD request")
		ddd := feedKeyFor("DDD", "1m")
		e.socketBars(e.owner, ddd, 40)
		release()
		fhWait(t, done, "the generation")
		want := map[string]string{
			"AAA": feedHistoryEmptyResponse,
			"BBB": "history rows were invalid",
			"CCC": "did not reach the venue history boundary",
			"DDD": feedHistoryEmptyResponse,
		}
		coins := []string{"AAA", "BBB", "CCC", "DDD"}
		for _, coin := range coins {
			key := feedKeyFor(coin, "1m")
			st := e.state(e.owner, key)
			r := e.ready(e.owner, key)
			if r.Ready || st.historyLookback != 0 || !strings.Contains(st.historyErr, want[coin]) || st.historyFailures != 1 {
				t.Fatalf("%s = %+v (history %d err %q failures %d), want not ready and not complete with %q", coin, r, st.historyLookback, st.historyErr, st.historyFailures, want[coin])
			}
		}
		if st := e.state(e.owner, ddd); len(st.Bars) < 40 {
			t.Fatalf("DDD kept %d socket bars, want the 40 that arrived during the request", len(st.Bars))
		}
		var kinds []string
		for _, a := range e.owner.DrainAlerts() {
			kinds = append(kinds, a.Kind+":"+a.Key.Symbol)
		}
		sort.Strings(kinds)
		if fmt.Sprint(kinds) != "[bootstrap_failed:AAA bootstrap_failed:BBB bootstrap_failed:CCC bootstrap_failed:DDD]" {
			t.Fatalf("alerts = %v, want one bootstrap failure per key", kinds)
		}
	})

	t.Run("ExhaustedShortHistoryCompletes", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		now := fhBase.UnixMilli()
		e.venue.setBoundary("AAA", now-40*fhStep)
		e.venue.setBoundary("BBB", now-10*fhStep)
		e.apply(e.owner, rlRequirements(100, 60, "AAA", "BBB"))
		aaa, bbb := feedKeyFor("AAA", "1m"), feedKeyFor("BBB", "1m")
		r := e.ready(e.owner, aaa)
		if !r.Ready || !r.CoverageShort || r.Required != 100 || r.Bars != 41 || !strings.Contains(r.Detail, "venue history is shorter") {
			t.Fatalf("AAA = %+v, want ready on confirmed short history with coverage_short", r)
		}
		r = e.ready(e.owner, bbb)
		if r.Ready || r.Status != feedStatusShort || r.HistoryLookback != 100 {
			t.Fatalf("BBB = %+v, want short below the ready floor with the boundary recorded", r)
		}
		sent := e.venue.count("BBB")
		e.clock.Advance(time.Hour)
		runFeedHistoryPass(t, e.owner, e.ctx)
		if got := e.venue.count("BBB"); got != sent {
			t.Fatalf("a key with a confirmed history boundary sent %d more full request(s)", got-sent)
		}
	})

	t.Run("PassLimitShortDoesNotComplete", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		e.venue.set("CCC", "grow")
		e.apply(e.owner, rlRequirements(100, 60, "CCC"))
		ccc := feedKeyFor("CCC", "1m")
		st := e.state(e.owner, ccc)
		if st.historyLookback != 0 || st.historyRetryAt.IsZero() || e.venue.count("CCC") != hlCandleMaxExtendPasses {
			t.Fatalf("CCC history %d retry %s requests %d, want incomplete and queued after %d passes", st.historyLookback, st.historyRetryAt, e.venue.count("CCC"), hlCandleMaxExtendPasses)
		}
		if r := e.ready(e.owner, ccc); r.Ready {
			t.Fatalf("CCC = %+v, want not ready", r)
		}
		e.clock.Advance(feedHistoryRetryBase)
		runFeedHistoryPass(t, e.owner, e.ctx)
		if got := e.venue.count("CCC"); got != 2*hlCandleMaxExtendPasses {
			t.Fatalf("CCC requests after one retry = %d, want another full search", got)
		}
	})

	t.Run("BlockedHistoryDoesNotBlockSealOrRepair", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		bKey, cKey := feedKeyFor("BBB", "1m"), feedKeyFor("CCC", "1m")
		e.apply(e.owner, rlRequirements(40, 60, "BBB", "CCC"))
		e.owner.SetConnected(true)
		e.clock.Advance(10 * time.Minute)
		upgrade := rlRequirements(40, 60, "BBB", "CCC")
		upgrade.Keys[bKey] = 80
		entered, release := e.venue.hold("BBB")
		defer release()
		done := e.owner.ApplyGeneration(e.ctx, upgrade)
		fhWait(t, entered, "the BBB history request")
		bSent := e.venue.count("BBB")
		cSent := e.venue.count("CCC")
		if r := e.ready(e.owner, cKey); !r.Stale {
			t.Fatalf("CCC = %+v, want stale before recovery", r)
		}
		for _, call := range []func() error{
			func() error { return e.owner.fetchAndMerge(e.ctx, bKey, feedRestRefresh) },
			func() error { return e.owner.fetchIncremental(e.ctx, bKey, feedRestRecovery) },
		} {
			if err := call(); !errors.Is(err, errFeedKeyInflight) {
				t.Fatalf("a second caller for BBB err = %v, want in flight", err)
			}
		}
		if jobs := e.owner.historyPass(feedHistoryWorkers); len(jobs) != 0 {
			t.Fatalf("history pass admitted %d job(s) while BBB is in flight", len(jobs))
		}
		prepared := make(chan struct{})
		go func() {
			prepareMarketSnapshot(e.ctx, e.owner, fullCycleRequirements(upgrade))
			close(prepared)
		}()
		fhWait(t, prepared, "seal preparation")
		reqs := e.venue.requests("CCC")
		if len(reqs) != cSent+1 || e.fullWindow(reqs[len(reqs)-1], 40) {
			t.Fatalf("CCC requests = %+v, want one short-window recovery", reqs[cSent:])
		}
		if r := e.ready(e.owner, cKey); !r.Ready {
			t.Fatalf("CCC after recovery = %+v, want ready", r)
		}
		e.owner.SetConnected(false)
		e.owner.SetConnected(true)
		repaired := make(chan struct{})
		go func() {
			e.owner.repairAfterConnect(e.ctx)
			close(repaired)
		}()
		fhWait(t, repaired, "reconnect repair")
		if r := e.ready(e.owner, cKey); r.Status != feedStatusReady {
			t.Fatalf("CCC after repair = %+v, want ready", r)
		}
		if r := e.ready(e.owner, bKey); r.Status != feedStatusRepairing {
			t.Fatalf("BBB after repair = %+v, want still repairing until history recovery provides verified inputs", r)
		}
		if got := e.venue.count("BBB"); got != bSent {
			t.Fatalf("BBB saw %d more request(s) while blocked, want none", got-bSent)
		}
		release()
		fhWait(t, done, "the generation")
		if r := e.ready(e.owner, bKey); !r.Ready || r.HistoryLookback != 80 || r.Required != 80 {
			t.Fatalf("BBB after its load = %+v, want ready on the 80-bar lookback", r)
		}
		for _, a := range e.owner.DrainAlerts() {
			t.Fatalf("unexpected alert %+v", a)
		}
	})

	t.Run("RateLimitPauseHoldsEveryKeyOnTheHost", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		bKey, cKey := feedKeyFor("BBB", "1m"), feedKeyFor("CCC", "1m")
		host := feedHostOf(e.venue.srv.URL)
		e.apply(e.owner, rlRequirements(40, 60, "BBB", "CCC"))
		e.clock.Advance(10 * time.Minute)
		e.venue.set("BBB", "429")
		upgrade := rlRequirements(40, 60, "BBB", "CCC")
		upgrade.Keys[bKey] = 80
		failedAt := e.clock.Now()
		e.apply(e.owner, upgrade)
		st := e.state(e.owner, bKey)
		if st.Status != feedStatusRateLimited || st.historyRetryAt.Sub(failedAt) < feedHistoryRefusalFloor {
			t.Fatalf("BBB status %s retry in %s, want rate_limited with at least %s", st.Status, st.historyRetryAt.Sub(failedAt), feedHistoryRefusalFloor)
		}
		events := e.ledger.rateLimitReport(feedRateLimitOutstanding(e.owner))
		if len(events) != 1 || events[0].Kind != feedRateLimitEventPaused {
			t.Fatalf("report = %+v, want one pause summary", events)
		}
		sentB, sentC := e.venue.count("BBB"), e.venue.count("CCC")
		prepareMarketSnapshot(e.ctx, e.owner, fullCycleRequirements(upgrade))
		if r := e.ready(e.owner, cKey); r.Ready {
			t.Fatalf("CCC during the host pause = %+v, want held", r)
		}
		e.clock.Advance(30 * time.Second)
		if errs := runFeedHistoryPass(t, e.owner, e.ctx); len(errs) != 0 {
			t.Fatalf("history pass inside the retry floor ran: %v", errs)
		}
		if e.venue.count("BBB") != sentB || e.venue.count("CCC") != sentC {
			t.Fatal("a request was sent during the host pause")
		}
		e.clock.Advance(30 * time.Second)
		if r := e.ready(e.owner, bKey); r.Status != feedStatusRateLimited {
			t.Fatalf("BBB after the pause with no fetch = %+v, want still rate_limited", r)
		}
		loopFailedAt := e.clock.Now()
		errs := runFeedHistoryPass(t, e.owner, e.ctx)
		if len(errs) != 1 || !errors.Is(errs[0], errHLInfoRateLimited) || e.venue.count("BBB") != sentB+1 {
			t.Fatalf("loop attempt after the floor errs %v sent %d, want one request answered by a 429", errs, e.venue.count("BBB")-sentB)
		}
		if st := e.state(e.owner, bKey); st.historyRetryAt.Sub(loopFailedAt) < feedHistoryRefusalFloor {
			t.Fatalf("loop retry in %s, want at least %s", st.historyRetryAt.Sub(loopFailedAt), feedHistoryRefusalFloor)
		}
		e.venue.set("BBB", "ok")
		e.clock.Advance(feedHistoryRefusalFloor)
		if errs := runFeedHistoryPass(t, e.owner, e.ctx); len(errs) != 0 {
			t.Fatalf("history pass after the pause: %v", errs)
		}
		prepareMarketSnapshot(e.ctx, e.owner, fullCycleRequirements(upgrade))
		for _, key := range []marketFeedKey{bKey, cKey} {
			if r := e.ready(e.owner, key); !r.Ready {
				t.Fatalf("%s after a later successful fetch = %+v, want ready", key, r)
			}
		}
		if h := rlHostState(e.ledger, host); h.InFlight != 0 {
			t.Fatalf("host state = %+v, want nothing in flight", h)
		}
		for _, a := range e.owner.DrainAlerts() {
			if a.Kind == feedHistoryAlertKind {
				t.Fatalf("a rate limit raised %+v", a)
			}
		}
	})

	t.Run("RetryBackoffFloorsAndCaps", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			jitter func(time.Duration) time.Duration
			want   []time.Duration
		}{
			{"zero_jitter", func(time.Duration) time.Duration { return 0 },
				[]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second}},
			{"max_jitter", func(span time.Duration) time.Duration { return span },
				[]time.Duration{7500 * time.Millisecond, 15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newFHEnv(t, fhBase)
				e.owner.historyJitter = tc.jitter
				e.venue.set("AAA", "500")
				key := feedKeyFor("AAA", "1m")
				req := rlRequirements(40, 60, "AAA")
				for i, want := range tc.want {
					at := e.clock.Now()
					if i == 0 {
						e.apply(e.owner, req)
					} else {
						runFeedHistoryPass(t, e.owner, e.ctx)
					}
					st := e.state(e.owner, key)
					if got := st.historyRetryAt.Sub(at); got != want || st.historyFailures != i+1 {
						t.Fatalf("failure %d: retry in %s with %d failure(s), want %s", i+1, got, st.historyFailures, want)
					}
					sent := e.venue.count("AAA")
					e.clock.Advance(want - time.Millisecond)
					e.apply(e.owner, req)
					e.owner.SetConnected(true)
					e.owner.SetConnected(false)
					e.owner.repairAfterConnect(e.ctx)
					prepareMarketSnapshot(e.ctx, e.owner, fullCycleRequirements(req))
					if err := e.owner.fetchAndMerge(e.ctx, key, feedRestRefresh); !errors.Is(err, errFeedHistoryNotDue) {
						t.Fatalf("refresh before the due time err = %v, want not due", err)
					}
					runFeedHistoryPass(t, e.owner, e.ctx)
					if got := e.venue.count("AAA"); got != sent {
						t.Fatalf("failure %d: %d request(s) sent before the due time", i+1, got-sent)
					}
					if st := e.state(e.owner, key); st.historyFailures != i+1 {
						t.Fatalf("not-due results changed the failure count to %d", st.historyFailures)
					}
					e.clock.Advance(time.Millisecond)
				}
				entered, release := e.venue.hold("AAA")
				jobs := e.owner.historyPass(feedHistoryWorkers)
				if len(jobs) != 1 {
					t.Fatalf("history pass = %d job(s), want 1", len(jobs))
				}
				finished := make(chan struct{})
				go func() {
					e.owner.runHistoryJob(e.ctx, jobs[0])
					close(finished)
				}()
				fhWait(t, entered, "the held request")
				failures := e.state(e.owner, key).historyFailures
				if err := e.owner.fetchAndMerge(e.ctx, key, feedRestRefresh); !errors.Is(err, errFeedKeyInflight) {
					t.Fatalf("refresh while in flight err = %v", err)
				}
				if st := e.state(e.owner, key); st.historyFailures != failures {
					t.Fatalf("an in-flight result changed the failure count from %d to %d", failures, st.historyFailures)
				}
				release()
				fhWait(t, finished, "the held job")
			})
		}

		refusals := []struct {
			name  string
			setup func(e *fhEnv)
			check func(t *testing.T, err string)
		}{
			{"http_429", func(e *fhEnv) { e.venue.set("AAA", "429") }, func(t *testing.T, err string) {
				if !strings.Contains(err, "rate limited") {
					t.Fatalf("history error %q, want the 429", err)
				}
			}},
			{"host_pause", func(e *fhEnv) {
				e.venue.set("ZZZ", "429")
				fetchHyperliquidCandleSnapshot(withFeedReason(e.ctx, string(feedRestRefresh)), "ZZZ", "1m", fhBase.Add(-time.Hour).UnixMilli(), fhBase.UnixMilli())
			}, func(t *testing.T, err string) {
				if !strings.Contains(err, errFeedHostPaused.Error()) {
					t.Fatalf("history error %q, want the host pause refusal", err)
				}
			}},
			{"request_budget", func(e *fhEnv) {
				e.ledger.setLimits(&feedBudgetConfig{PerMinute: 1, Startup: 0})
				e.ledger.mu.Lock()
				e.ledger.enforce = true
				e.ledger.mu.Unlock()
				if _, err := fetchHyperliquidCandleSnapshot(withFeedReason(e.ctx, string(feedRestRefresh)), "ZZZ", "1m", fhBase.Add(-time.Hour).UnixMilli(), fhBase.UnixMilli()); err != nil {
					e.t.Fatalf("the read that uses the only budget slot: %v", err)
				}
			}, func(t *testing.T, err string) {
				if !strings.Contains(err, errFeedBudgetExhausted.Error()) {
					t.Fatalf("history error %q, want the request budget refusal", err)
				}
			}},
			{"local_weight", func(e *fhEnv) {
				e.ledger.setLimits((&feedWeightBudgetConfig{WeightPerMinute: feedIntPtr(0), WeightStartup: feedIntPtr(0), WeightReserve: feedIntPtr(0)}).budget())
			}, func(t *testing.T, err string) {
				if !strings.Contains(err, errFeedWeightRefused.Error()) {
					t.Fatalf("history error %q, want the weight refusal", err)
				}
			}},
		}
		for _, rc := range refusals {
			t.Run(rc.name, func(t *testing.T) {
				e := newFHEnv(t, fhBase)
				rc.setup(e)
				at := e.clock.Now()
				e.apply(e.owner, rlRequirements(40, 60, "AAA"))
				st := e.state(e.owner, feedKeyFor("AAA", "1m"))
				rc.check(t, st.historyErr)
				if got := st.historyRetryAt.Sub(at); got < feedHistoryRefusalFloor || st.historyLookback != 0 {
					t.Fatalf("retry in %s with history %d, want at least %s and no completion", got, st.historyLookback, feedHistoryRefusalFloor)
				}
			})
		}
	})

	t.Run("ObsoleteResultsAreDropped", func(t *testing.T) {
		for _, outcome := range []string{"ok", "500", "429"} {
			t.Run(outcome, func(t *testing.T) {
				e := newFHEnv(t, fhBase)
				key := feedKeyFor("XXX", "1m")
				req := rlRequirements(40, 60, "XXX")
				e.apply(e.owner, req)
				oldEntered, oldRelease := e.venue.hold("XXX")
				defer oldRelease()
				oldDone := make(chan error, 1)
				go func() { oldDone <- e.owner.fetchAndMerge(e.ctx, key, feedRestRefresh) }()
				fhWait(t, oldEntered, "the old request")
				empty := feedRequirements{Keys: map[marketFeedKey]int{}, Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}}
				empty.finalize()
				e.apply(e.owner, empty)
				newEntered, newRelease := e.venue.hold("XXX")
				defer newRelease()
				readd := e.owner.ApplyGeneration(e.ctx, req)
				fhWait(t, newEntered, "the replacement bootstrap")
				before := e.state(e.owner, key)
				e.venue.set("XXX", outcome)
				oldRelease()
				var err error
				select {
				case err = <-oldDone:
				case <-time.After(10 * time.Second):
					t.Fatal("old request did not finish")
				}
				if !errors.Is(err, errFeedKeyObsolete) {
					t.Fatalf("old result err = %v, want obsolete", err)
				}
				after := e.state(e.owner, key)
				if len(after.Bars) != 0 || after.Status != before.Status || after.StatusDetail != before.StatusDetail || after.rlSeq != 0 ||
					!after.historyRetryAt.IsZero() || after.historyFailures != 0 || after.historyLookback != 0 || after.historyErr != "" ||
					after.weightHeld || after.sharedHeld || !after.historyInflight {
					t.Fatalf("replacement after the obsolete %s result = %+v, want it untouched", outcome, after)
				}
				h := rlHostState(e.ledger, feedHostOf(e.venue.srv.URL))
				if outcome == "429" && h.State != feedRateLimitStatePaused {
					t.Fatalf("host state = %+v, want the transport pause kept", h)
				}
				if h.InFlight != 1 {
					t.Fatalf("host in flight = %d, want only the replacement request", h.InFlight)
				}
				if w := e.ledger.weightStatus(); w.Pending != 0 {
					t.Fatalf("weight status = %+v, want nothing pending", w)
				}
				e.venue.set("XXX", "ok")
				newRelease()
				fhWait(t, readd, "the replacement generation")
				if h := rlHostState(e.ledger, feedHostOf(e.venue.srv.URL)); h.InFlight != 0 {
					t.Fatalf("host state = %+v, want nothing in flight", h)
				}
			})
		}
	})

	t.Run("LookbackRaiseDuringFlight", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		key := feedKeyFor("XXX", "1m")
		req := func(n int) feedRequirements { return rlRequirements(n, 60, "XXX") }
		e.apply(e.owner, req(40))
		entered, release := e.venue.hold("XXX")
		defer release()
		first := e.owner.ApplyGeneration(e.ctx, req(60))
		fhWait(t, entered, "the 60-bar load")
		sent := e.venue.count("XXX")
		second := e.owner.ApplyGeneration(e.ctx, req(80))
		fhWait(t, second, "the second generation while the first load is blocked")
		if got := e.venue.count("XXX"); got != sent {
			t.Fatalf("the second generation sent %d request(s) for an in-flight key", got-sent)
		}
		release()
		fhWait(t, first, "the first generation")
		st := e.state(e.owner, key)
		r := e.ready(e.owner, key)
		if st.historyLookback != 60 || st.historyTarget != 80 || !r.Ready || r.Required != 60 || !strings.Contains(r.Detail, "80-bar load is pending") {
			t.Fatalf("after the captured load: history %d target %d readiness %+v, want 60 covered and 80 queued", st.historyLookback, st.historyTarget, r)
		}
		if errs := runFeedHistoryPass(t, e.owner, e.ctx); len(errs) != 0 {
			t.Fatal(errs)
		}
		if r := e.ready(e.owner, key); r.HistoryLookback != 80 || r.Required != 80 || !r.Ready {
			t.Fatalf("after the queued load = %+v, want 80 covered", r)
		}
	})

	t.Run("SameOrLowerReloadKeepsCompletion", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		key := feedKeyFor("XXX", "1m")
		e.apply(e.owner, rlRequirements(60, 60, "XXX"))
		sent := e.venue.count("XXX")
		e.apply(e.owner, rlRequirements(60, 60, "XXX"))
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		if got := e.venue.count("XXX"); got != sent {
			t.Fatalf("same or lower reload sent %d request(s)", got-sent)
		}
		if r := e.ready(e.owner, key); !r.Ready || r.HistoryLookback != 60 || r.Required != 60 {
			t.Fatalf("after a lower reload = %+v, want the 60-bar completion kept", r)
		}
	})

	t.Run("RaisedThenLowered", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		key := feedKeyFor("XXX", "1m")
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		e.venue.set("XXX", "500")
		e.apply(e.owner, rlRequirements(80, 60, "XXX"))
		if r := e.ready(e.owner, key); !r.Ready || r.Required != 40 {
			t.Fatalf("after a failed raise = %+v, want ready on the old 40-bar coverage", r)
		}
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		st := e.state(e.owner, key)
		if st.historyRetryAt.IsZero() || st.historyTarget != 40 {
			t.Fatalf("after lowering: retry %s target %d, want the retry time kept and the target lowered", st.historyRetryAt, st.historyTarget)
		}
		e.venue.set("XXX", "ok")
		sent := e.venue.count("XXX")
		if err := e.owner.fetchIncremental(e.ctx, key, feedRestRecovery); err != nil {
			t.Fatalf("incremental read after lowering: %v", err)
		}
		reqs := e.venue.requests("XXX")
		if len(reqs) != sent+1 || e.fullWindow(reqs[len(reqs)-1], 40) {
			t.Fatalf("requests after lowering = %+v, want one short window", reqs[sent:])
		}
	})

	t.Run("RemoveAndReAdd", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		key := feedKeyFor("XXX", "1m")
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		empty := feedRequirements{Keys: map[marketFeedKey]int{}, Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}}
		empty.finalize()
		e.apply(e.owner, empty)
		e.venue.set("XXX", "500")
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		if st := e.state(e.owner, key); st.historyLookback != 0 {
			t.Fatalf("re-added key inherited history %d", st.historyLookback)
		}
		if r := e.ready(e.owner, key); r.Ready {
			t.Fatalf("re-added key = %+v, want not ready", r)
		}
	})

	t.Run("ConsumersDuringUpgrade", func(t *testing.T) {
		start := time.Now().UTC().Truncate(time.Minute)
		e := newFHEnv(t, start)
		btc, eth := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m")
		need := func(b, eh int) feedRequirements {
			return keyFillConsumer(map[marketFeedKey]int{btc: b, eth: eh})
		}
		e.apply(e.owner, need(20, 100))
		e.venue.set("BTC", "500")
		e.venue.set("ETH", "500")
		e.apply(e.owner, need(100, 200))
		for key, covered := range map[marketFeedKey]int{btc: 20, eth: 100} {
			if r := e.ready(e.owner, key); !r.Ready || r.Required != covered {
				t.Fatalf("%s during the upgrade = %+v, want ready on the %d-bar coverage", key, r, covered)
			}
		}
		backup := e.newOwner()
		e.venue.set("BTC", "ok")
		e.venue.set("ETH", "ok")
		e.apply(backup, need(100, 200))
		e.venue.set("BTC", "500")
		e.venue.set("ETH", "500")

		dir, err := os.MkdirTemp("", "gfh")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		k := start.Unix()
		sockets := map[string]string{}
		for name, o := range map[string]*marketFeedOwner{"primary": e.owner, "backup": backup} {
			s := newFeedSealer(o, feedSourceWebsocket, "inst-"+name, e.clock.Now, e.logf)
			s.settle, s.prepare = 0, time.Hour
			s.prepareFn = websocketFeedPrepare(o)
			s.setGeneration(need(100, 200), []int{60}, start.Add(-30*time.Minute))
			s.startServing(start.Add(-30 * time.Minute))
			s.sealOne(e.ctx, k)
			if s.sealBytes(k) == nil {
				t.Fatalf("%s did not seal %d", name, k)
			}
			path := dir + "/" + name + ".sock"
			srv, err := listenFeedSocket(path, s, e.logf)
			if err != nil {
				t.Fatal(err)
			}
			go srv.serve()
			t.Cleanup(srv.close)
			sockets[name] = path
		}
		client, _ := keyFillClient(sockets["primary"], sockets["backup"])
		covered := need(20, 100)
		snap, rep := client.Fetch(context.Background(), k, keyFillCycle(covered))
		if rep.Status != feedFetchSealed || rep.Endpoint != "primary" || len(rep.Gaps) != 0 || len(rep.Filled) != 0 {
			t.Fatalf("covered consumer report = %+v, want the primary seal with no gap or fill", rep)
		}
		ctxCovered := &marketFeedContext{Enabled: true, Requirements: covered, Snapshot: snap}
		for _, id := range []string{"s-BTC", "s-ETH"} {
			if hold := ctxCovered.holdFor(StrategyConfig{ID: id}); hold.Held {
				t.Fatalf("%s within coverage held: %+v", id, hold)
			}
			if _, err := ctxCovered.singleCheckPayload(StrategyConfig{ID: id}); err != nil {
				t.Fatalf("%s within coverage payload: %v", id, err)
			}
		}
		larger := need(100, 200)
		snap, rep = client.Fetch(context.Background(), k, keyFillCycle(larger))
		if rep.Endpoint != "primary" || len(rep.Filled) != 0 || len(rep.Gaps) != 2 {
			t.Fatalf("larger consumer report = %+v, want the primary kept, no fill and two coverage gaps", rep)
		}
		if got := unreadyCandleKeys(snap, []marketFeedKey{btc, eth}); len(got) != 2 {
			t.Fatalf("larger consumer unready keys = %v, want both held by the coverage gate", got)
		}
		for _, id := range []string{"s-BTC", "s-ETH"} {
			if hold := (&marketFeedContext{Enabled: true, Requirements: larger, Snapshot: snap}).holdFor(StrategyConfig{ID: id}); !hold.Held {
				t.Fatalf("%s needing more than the coverage was not held", id)
			}
		}

		embedded := sealCycleMarketSnapshot(e.ctx, e.owner, keyFillCycle(larger), "embedded", e.clock.Now())
		ctxLarger := &marketFeedContext{Enabled: true, Requirements: larger, Snapshot: embedded}
		hold := ctxLarger.holdFor(StrategyConfig{ID: "s-BTC"})
		if !hold.Held || hold.Key != btc || !strings.Contains(hold.Reason, "covers a 20-bar history lookback, below the 100") {
			t.Fatalf("embedded BTC hold = %+v, want a coverage hold", hold)
		}
		if _, err := marketPayloadFor(embedded, []marketPayloadFrameSpec{{Key: btc, Required: 100}}, nil); err == nil || !strings.Contains(err.Error(), "covers a 20-bar history lookback") {
			t.Fatalf("embedded payload for 100 bars err = %v, want the coverage error", err)
		}
		if p, err := marketPayloadFor(embedded, []marketPayloadFrameSpec{{Key: btc, Required: 20}, {Key: eth, Required: 200, Optional: true}}, nil); err != nil || len(p.Frames) != 1 {
			t.Fatalf("embedded payload within coverage err = %v frames %d, want BTC only with the optional frame skipped", err, len(p.Frames))
		}
	})

	t.Run("RESTDueKeyInFlightOrDeferred", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		key := feedKeyFor("XXX", "1m")
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		e.venue.set("XXX", "500")
		raised := rlRequirements(80, 60, "XXX")
		k := fhBase.Unix()/60*60 + 120
		at := func(deadline int64) {
			e.clock.mu.Lock()
			e.clock.now = time.Unix(deadline, 0).UTC().Add(time.Second)
			e.clock.mu.Unlock()
		}
		e.clock.mu.Lock()
		e.clock.now = time.Unix(k, 0).UTC().Add(-2 * time.Second)
		e.clock.mu.Unlock()
		e.apply(e.owner, raised)
		s := newFeedSealer(e.owner, feedSourceREST, "inst-rest", e.clock.Now, e.logf)
		s.ledger = e.ledger
		src := newFeedRESTSource(e.owner, e.logf)
		src.backoff = time.Millisecond
		s.prepareFn = src.prepare
		s.setGeneration(raised, []int{60}, fhBase.Add(-time.Hour))
		sealReadiness := func(k int64) feedSealReadiness {
			t.Helper()
			doc, err := decodeFeedSeal(s.sealBytes(k), k)
			if err != nil {
				t.Fatal(err)
			}
			for _, sk := range doc.Keys {
				if sk.Symbol == "XXX" {
					return sk.Readiness
				}
			}
			t.Fatalf("seal %d has no XXX key", k)
			return feedSealReadiness{}
		}

		at(k)
		before := e.state(e.owner, key)
		sent := e.venue.count("XXX")
		s.sealOne(e.ctx, k)
		r := sealReadiness(k)
		if r.Ready || !strings.Contains(r.Detail, "full-history retry is due at") || e.venue.count("XXX") != sent {
			t.Fatalf("deferred seal readiness %+v sent %d, want not ready with the retry time and no request", r, e.venue.count("XXX")-sent)
		}
		if after := e.state(e.owner, key); after.Status != before.Status || after.StatusDetail != before.StatusDetail || after.historyFailures != before.historyFailures {
			t.Fatalf("deferred result changed the stored key from %+v to %+v", before, after)
		}

		k2 := k + 60
		at(k2)
		sent = e.venue.count("XXX")
		s.sealOne(e.ctx, k2)
		reqs := e.venue.requests("XXX")
		if len(reqs) != sent+1 || !e.fullWindow(reqs[len(reqs)-1], 80) {
			t.Fatalf("due refresh requests = %+v, want one full 80-bar request and no immediate retry", reqs[sent:])
		}
		if r := sealReadiness(k2); r.Ready {
			t.Fatalf("seal after a failed full load = %+v, want not ready", r)
		}

		e.venue.set("XXX", "ok")
		e.clock.Advance(feedHistoryRetryCap)
		entered, release := e.venue.hold("XXX")
		defer release()
		boot := e.owner.ApplyGeneration(e.ctx, raised)
		fhWait(t, entered, "the held bootstrap")
		k3 := k2 + 600
		at(k3)
		sent = e.venue.count("XXX")
		statusBefore := e.state(e.owner, key).Status
		s.sealOne(e.ctx, k3)
		r = sealReadiness(k3)
		if r.Ready || !strings.Contains(r.Detail, "a history request for this key was in flight") || e.venue.count("XXX") != sent {
			t.Fatalf("in-flight seal readiness %+v sent %d, want not ready with the in-flight text and no request", r, e.venue.count("XXX")-sent)
		}
		if got := e.state(e.owner, key).Status; got != statusBefore {
			t.Fatalf("in-flight result changed the stored status from %s to %s", statusBefore, got)
		}
		sealedBytes := append([]byte(nil), s.sealBytes(k3)...)
		release()
		fhWait(t, boot, "the concurrent bootstrap")
		if got := e.ready(e.owner, key); got.HistoryLookback != 80 {
			t.Fatalf("owner after the bootstrap = %+v, want 80 covered", got)
		}
		if got := s.sealBytes(k3); string(got) != string(sealedBytes) || sealReadiness(k3).Ready {
			t.Fatal("a concurrent bootstrap completion changed the deadline seal or marked it refreshed")
		}
		k4 := k3 + 60
		at(k4)
		s.sealOne(e.ctx, k4)
		if r := sealReadiness(k4); !r.Ready || r.Required != 80 {
			t.Fatalf("seal after the load = %+v, want ready on the 80-bar lookback", r)
		}
	})

	t.Run("LoopLifecycle", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		e.owner.historyTick = 5 * time.Millisecond
		e.venue.set("XXX", "500")
		e.apply(e.owner, rlRequirements(40, 60, "XXX"))
		sent := e.venue.count("XXX")
		ctx, cancel := context.WithCancel(e.ctx)
		stopped := make(chan struct{})
		go func() {
			e.owner.Run(ctx)
			close(stopped)
		}()
		e.clock.Advance(feedHistoryRetryBase)
		deadline := time.Now().Add(10 * time.Second)
		for e.venue.count("XXX") == sent {
			if time.Now().After(deadline) {
				t.Fatal("the history loop under Run sent no retry")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		fhWait(t, stopped, "Run to stop")
		time.Sleep(50 * time.Millisecond)
		after := e.venue.count("XXX")
		e.clock.Advance(time.Hour)
		time.Sleep(50 * time.Millisecond)
		if got := e.venue.count("XXX"); got != after {
			t.Fatalf("the history loop sent %d request(s) after its context ended", got-after)
		}
		alerted := false
		for _, a := range e.owner.DrainAlerts() {
			if a.Kind == feedHistoryAlertKind {
				alerted = true
			}
		}
		if !alerted {
			t.Fatal("a failed loop attempt raised no history_failed alert")
		}

		rest := e.newOwner()
		e.venue.set("YYY", "500")
		e.apply(rest, rlRequirements(40, 60, "YYY"))
		restSent := e.venue.count("YYY")
		e.clock.Advance(time.Hour)
		time.Sleep(50 * time.Millisecond)
		if got := e.venue.count("YYY"); got != restSent {
			t.Fatalf("an owner without Run sent %d request(s) between deadlines", got-restSent)
		}

		bare := e.newOwner()
		bare.historyTick = 5 * time.Millisecond
		bctx, bcancel := context.WithCancel(e.ctx)
		bstopped := make(chan struct{})
		total := func() int {
			n := 0
			for _, coin := range []string{"XXX", "YYY"} {
				n += e.venue.count(coin)
			}
			return n
		}
		base := total()
		go func() {
			bare.Run(bctx)
			close(bstopped)
		}()
		time.Sleep(50 * time.Millisecond)
		bcancel()
		fhWait(t, bstopped, "the bare owner to stop")
		if got := total(); got != base {
			t.Fatalf("an owner with no candle keys sent %d candle request(s)", got-base)
		}
	})

	t.Run("StatusSurfaces", func(t *testing.T) {
		e := newFHEnv(t, fhBase)
		e.venue.set("BBB", "500")
		e.apply(e.owner, rlRequirements(40, 60, "CCC", "BBB", "AAA"))
		health := e.owner.Health("")
		var names []string
		for _, k := range health.Keys {
			names = append(names, k.Key)
		}
		if !sort.StringsAreSorted(names) || len(names) != 3 {
			t.Fatalf("health keys = %v, want three sorted keys", names)
		}
		for _, k := range health.Keys {
			failed := strings.Contains(k.Key, "|BBB|")
			switch {
			case failed && (k.Ready || k.HistoryLookback != 0 || k.HistoryTarget != 40 || k.HistoryError == ""):
				t.Fatalf("BBB health = %+v, want not ready with target 40 and the history error", k)
			case !failed && (!k.Ready || k.HistoryLookback != 40 || k.HistoryTarget != 40 || k.HistoryError != "" || k.Required != 40):
				t.Fatalf("%s health = %+v, want ready with 40 covered", k.Key, k)
			}
		}
	})

	t.Run("SharedWeightHoldAtRestart", func(t *testing.T) {
		fa := newFAEnv(t)
		venue := newFHVenue(t)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		a := fa.owner("a", wtPolicy(600, 100, 100))
		fa.settle(a)
		a.a.sharedReport()
		if err := os.Remove(filepath.Join(fa.dir, feedAuthorityRecordName)); err != nil {
			t.Fatal(err)
		}
		a.a.heartbeatOnce(context.Background())
		owner := newMarketFeedOwner(fa.clock.Wall, fa.logf)
		owner.SetRequestLedger(a.l)
		owner.historyJitter = func(time.Duration) time.Duration { return 0 }
		ctx, cancel := context.WithTimeout(withFeedLedger(context.Background(), a.l), 2*time.Second)
		defer cancel()
		at := fa.clock.Wall()
		<-owner.ApplyGeneration(ctx, rlRequirements(40, 60, "BTC"))
		key := feedKeyFor("BTC", "1m")
		owner.feedMu.Lock()
		st := *owner.keys[key]
		owner.feedMu.Unlock()
		if st.historyLookback != 0 || !strings.Contains(st.historyErr, feedWeightCauseSharedHold) {
			t.Fatalf("bootstrap during the shared hold: history %d err %q, want a shared refusal", st.historyLookback, st.historyErr)
		}
		if got := st.historyRetryAt.Sub(at); got < feedHistoryRefusalFloor {
			t.Fatalf("retry in %s, want at least %s", got, feedHistoryRefusalFloor)
		}
		if r, _ := owner.readinessFor(key); r.Ready {
			t.Fatalf("key = %+v, want not ready", r)
		}
		if venue.count("BTC") != 0 {
			t.Fatalf("the venue saw %d request(s) during the shared hold", venue.count("BTC"))
		}
		fa.settle(a)
		fa.clock.advance(feedHistoryRefusalFloor)
		a.a.heartbeatOnce(context.Background())
		if errs := runFeedHistoryPass(t, owner, withFeedLedger(context.Background(), a.l)); len(errs) != 0 {
			t.Fatalf("history pass after the hold: %v", errs)
		}
		if r, _ := owner.readinessFor(key); !r.Ready || r.HistoryLookback != 40 {
			t.Fatalf("key after the hold = %+v, want ready", r)
		}
	})
}
