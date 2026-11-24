package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

type rlVenue struct {
	srv    *httptest.Server
	clock  func() time.Time
	mu     sync.Mutex
	status map[string]int
	counts map[string]int
	gate   map[string]chan struct{}
	enter  map[string]chan struct{}
}

func newRLVenue(t *testing.T, clock func() time.Time) *rlVenue {
	t.Helper()
	v := &rlVenue{clock: clock, status: map[string]int{}, counts: map[string]int{}, gate: map[string]chan struct{}{}, enter: map[string]chan struct{}{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *rlVenue) setStatus(reqType string, status int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.status[reqType] = status
}

func (v *rlVenue) count(reqType string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.counts[reqType]
}

func (v *rlVenue) hold(reqType string) (entered <-chan struct{}, release func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	g, e := make(chan struct{}), make(chan struct{}, 1)
	v.gate[reqType], v.enter[reqType] = g, e
	return e, func() { close(g) }
}

func (v *rlVenue) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		Req  struct {
			StartTime int64 `json:"startTime"`
			EndTime   int64 `json:"endTime"`
		} `json:"req"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	v.mu.Lock()
	v.counts[req.Type]++
	status := v.status[req.Type]
	gate, enter := v.gate[req.Type], v.enter[req.Type]
	delete(v.gate, req.Type)
	delete(v.enter, req.Type)
	v.mu.Unlock()
	if gate != nil {
		enter <- struct{}{}
		<-gate
		v.mu.Lock()
		status = v.status[req.Type]
		v.mu.Unlock()
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Type {
	case "candleSnapshot":
		const step = int64(60_000)
		first := (req.Req.StartTime + step - 1) / step * step
		rows := []map[string]any{}
		for open := first; open <= req.Req.EndTime && len(rows) < 5000; open += step {
			rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"})
		}
		json.NewEncoder(w).Encode(rows)
	case "allMids":
		w.Write([]byte(`{"BTC":"101.5","ETH":"11"}`))
	case "meta":
		w.Write([]byte(hlMetaBody(`{"name":"BTC","szDecimals":5}`)))
	default:
		w.Write([]byte(`[]`))
	}
}

func rlRequirements(lookback int, cadence int, coins ...string) feedRequirements {
	req := feedRequirements{Keys: map[marketFeedKey]int{}, Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}, KeyCadences: map[marketFeedKey][]int{}}
	for _, coin := range coins {
		key := feedKeyFor(coin, "1m")
		req.Keys[key] = lookback
		req.KeyCadences[key] = []int{cadence}
		req.Strategies["s-"+coin] = feedStrategyRequirement{ID: "s-" + coin, Signal: key, SignalLookback: lookback, Coin: coin}
	}
	req.MidCoins = []string{coins[0]}
	req.finalize()
	return req
}

func rlKeyStatus(t *testing.T, o *marketFeedOwner, key marketFeedKey) feedKeyReadiness {
	t.Helper()
	r, ok := o.readinessFor(key)
	if !ok {
		t.Fatalf("key %s is not tracked", key)
	}
	return r
}

func rlHostState(l *feedRequestLedger, host string) feedRateLimitHostStatus {
	for _, h := range l.status().RateLimit {
		if h.Host == host {
			return h
		}
	}
	return feedRateLimitHostStatus{Host: host, State: "absent"}
}

func TestFeedRateLimitPause(t *testing.T) {
	base := time.Unix(1890000000, 0).UTC()

	t.Run("ReadersClassify429", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		for _, typ := range []string{"candleSnapshot", "allMids", "metaAndAssetCtxs", "fundingHistory", "meta"} {
			venue.setStatus(typ, http.StatusTooManyRequests)
		}
		ctx := context.Background()
		readers := map[string]func() error{
			"candleSnapshot": func() error {
				_, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli())
				return err
			},
			"allMids": func() error {
				_, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"})
				return err
			},
			"metaAndAssetCtxs": func() error {
				_, err := fetchHyperliquidFundingContexts(ctx)
				return err
			},
			"fundingHistory": func() error {
				_, err := hlFundingRecordsSince(ctx, "BTC", base.Add(-time.Hour).UnixMilli(), base)
				return err
			},
			"meta": func() error {
				_, err := fetchHLLotMetadataRaw(ctx, venue.srv.URL)
				return err
			},
		}
		names := make([]string, 0, len(readers))
		for name := range readers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for attempt := 1; attempt <= 2; attempt++ {
				if err := readers[name](); !errors.Is(err, errHLInfoRateLimited) {
					t.Fatalf("%s attempt %d: err = %v, want errHLInfoRateLimited", name, attempt, err)
				}
				if got := venue.count(name); got != attempt {
					t.Fatalf("%s: a read with no ledger sent %d request(s) after %d attempt(s); it must keep sending as before", name, got, attempt)
				}
			}
		}
	})

	t.Run("LedgerPauseLifecycle", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		host := feedHostOf(venue.srv.URL)
		ledger := newFeedRequestLedger(&feedBudgetConfig{PerMinute: 100, Startup: 5}, true, clock.Now)
		ctx := withFeedReason(withFeedLedger(context.Background(), ledger), string(feedRestRefresh))

		early, err := ledger.acquire(host, "refresh", "candleSnapshot", "early", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := early.beginSend(); err != nil {
			t.Fatal(err)
		}
		venue.setStatus("candleSnapshot", http.StatusTooManyRequests)
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli()); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("first read err = %v, want errHLInfoRateLimited", err)
		}
		if got := ledger.pausedUntil(host); !got.Equal(base.Add(feedRateLimitPause)) {
			t.Fatalf("pause end = %s, want %s", got, base.Add(feedRateLimitPause))
		}
		venue.setStatus("candleSnapshot", http.StatusOK)

		clock.Advance(20 * time.Second)
		early.finish(http.StatusTooManyRequests, "30")
		extended := base.Add(20*time.Second + feedRateLimitPause)
		if got := ledger.pausedUntil(host); !got.Equal(extended) {
			t.Fatalf("pause end after an in-flight 429 = %s, want the extension to %s", got, extended)
		}

		ledger.setLimits(&feedBudgetConfig{PerMinute: 500, Startup: 50})
		ledger.openStartup()
		if got := ledger.pausedUntil(host); !got.Equal(extended) {
			t.Fatalf("pause end after setLimits+openStartup = %s, want %s unchanged", got, extended)
		}
		bootCtx := withFeedReason(withFeedLedger(context.Background(), ledger), string(feedRestBootstrap))
		for _, c := range []context.Context{ctx, bootCtx} {
			if _, err := fetchHyperliquidCandleSnapshot(c, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli()); !errors.Is(err, errFeedHostPaused) {
				t.Fatalf("read during the pause err = %v, want errFeedHostPaused", err)
			}
		}
		if got := venue.count("candleSnapshot"); got != 1 {
			t.Fatalf("venue saw %d candle request(s) during the pause, want only the first", got)
		}
		st := rlHostState(ledger, host)
		if st.State != feedRateLimitStatePaused || st.Extensions != 1 || st.Refused != 2 || st.LastRetryAfter != `"30"` {
			t.Fatalf("host status = %+v, want paused with 1 extension, 2 refusals, retry instruction \"30\"", st)
		}

		clock.Advance(feedRateLimitPause - time.Second)
		if _, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"}); !errors.Is(err, errFeedHostPaused) {
			t.Fatalf("read one second before the end err = %v, want errFeedHostPaused", err)
		}
		clock.Advance(time.Second)
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli()); err != nil {
			t.Fatalf("read at the pause end on the ledger clock: %v", err)
		}
		if got := venue.count("candleSnapshot"); got != 2 {
			t.Fatalf("venue candle requests = %d, want 2 after expiry", got)
		}
		totals := ledger.snapshot()
		if totals.RateLimited != 2 || totals.Paused != 3 || totals.Refused != 0 {
			t.Fatalf("totals = %+v, want 2 rate-limited, 3 paused, 0 budget refusals", totals)
		}
	})

	t.Run("HostIsolation", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		mainnet := newRLVenue(t, clock.Now)
		testnet := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, mainnet.srv.URL, testnet.srv.URL)
		t.Setenv("HYPERLIQUID_TESTNET", "1")
		testnet.setStatus("meta", http.StatusTooManyRequests)
		ledger := newFeedRequestLedger(nil, false, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		lots := newHLLotMetadataCache(ctx)
		lots.clock = clock.Now
		lots.Ensure("BTC")
		if testnet.count("meta") != 1 || lots.requestLedger() != ledger {
			t.Fatalf("testnet meta requests = %d, shared ledger = %t; want one request through the feed owner ledger", testnet.count("meta"), lots.requestLedger() == ledger)
		}
		if st := rlHostState(ledger, feedHostOf(testnet.srv.URL)); st.State != feedRateLimitStatePaused {
			t.Fatalf("testnet host state = %+v, want paused", st)
		}
		req := rlRequirements(100, 60, "BTC")
		<-owner.ApplyGeneration(ctx, req)
		if r := rlKeyStatus(t, owner, feedKeyFor("BTC", "1m")); !r.Ready {
			t.Fatalf("mainnet key = %+v, want ready while only the testnet host is paused", r)
		}
		if _, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"}); err != nil {
			t.Fatalf("mainnet mids during a testnet pause: %v", err)
		}
		if mainnet.count("candleSnapshot") != 1 || mainnet.count("allMids") != 1 {
			t.Fatalf("mainnet requests = %d candle, %d mids; want both sent", mainnet.count("candleSnapshot"), mainnet.count("allMids"))
		}
		if st := rlHostState(ledger, feedHostOf(mainnet.srv.URL)); st.State != feedRateLimitStateClear {
			t.Fatalf("mainnet host state = %+v, want clear", st)
		}
		health := owner.Health("")
		if len(health.RateLimit) != 2 || health.RateLimit[0].Host > health.RateLimit[1].Host {
			t.Fatalf("embedded health rate_limit = %+v, want both hosts sorted", health.RateLimit)
		}
	})

	t.Run("AdmittedButUnsentRefuses", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		ledger := newFeedRequestLedger(&feedBudgetConfig{PerMinute: 100, Startup: 1}, true, clock.Now)
		ctx := withFeedInput(withFeedReason(withFeedLedger(context.Background(), ledger), string(feedRestMids)), "mids")
		admitted, release := make(chan struct{}), make(chan struct{})
		hlInfoBeforeSendHook = func(_ context.Context, reqType string) {
			if reqType == "allMids" {
				close(admitted)
				<-release
			}
		}
		t.Cleanup(func() { hlInfoBeforeSendHook = nil })
		done := make(chan error, 1)
		go func() {
			_, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"})
			done <- err
		}()
		<-admitted
		venue.setStatus("candleSnapshot", http.StatusTooManyRequests)
		candleCtx := withFeedInput(withFeedReason(withFeedLedger(context.Background(), ledger), string(feedRestRefresh)), "BTC|1m")
		if _, err := fetchHyperliquidCandleSnapshot(candleCtx, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli()); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("candle err = %v, want errHLInfoRateLimited", err)
		}
		close(release)
		err := <-done
		var paused *feedHostPausedError
		if !errors.As(err, &paused) || !paused.Until.Equal(base.Add(feedRateLimitPause)) {
			t.Fatalf("admitted-but-unsent read err = %v, want a pause refusal until %s", err, base.Add(feedRateLimitPause))
		}
		if got := venue.count("allMids"); got != 0 {
			t.Fatalf("venue saw %d mids request(s), want 0: the admitted read must not send into the pause", got)
		}
		st := ledger.status()
		if st.WindowUsed != 2 || st.Totals.Total != 2 || st.Totals.Paused != 1 || st.Totals.PausedByReason[string(feedRestMids)] != 1 {
			t.Fatalf("ledger = %+v, want both admission slots counted and one mids pause refusal", st)
		}
		if h := rlHostState(ledger, feedHostOf(venue.srv.URL)); h.InFlight != 0 || len(h.RefusedInputs) != 2 {
			t.Fatalf("host state = %+v, want no request in flight and refused inputs [BTC|1m mids]", h)
		}
	})

	t.Run("InFlightFinishesAndExtends", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		ledger := newFeedRequestLedger(nil, false, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		host := feedHostOf(venue.srv.URL)
		entered, release := venue.hold("candleSnapshot")
		done := make(chan error, 1)
		go func() {
			_, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-time.Hour).UnixMilli(), base.UnixMilli())
			done <- err
		}()
		<-entered
		venue.setStatus("allMids", http.StatusTooManyRequests)
		if _, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"}); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("mids err = %v, want errHLInfoRateLimited", err)
		}
		if h := rlHostState(ledger, host); h.InFlight != 1 || h.State != feedRateLimitStatePaused {
			t.Fatalf("host state = %+v, want paused with the candle request in flight", h)
		}
		clock.Advance(10 * time.Second)
		venue.setStatus("candleSnapshot", http.StatusTooManyRequests)
		release()
		if err := <-done; !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("in-flight read err = %v, want its own 429 recorded", err)
		}
		if got := ledger.pausedUntil(host); !got.Equal(base.Add(10*time.Second + feedRateLimitPause)) {
			t.Fatalf("pause end = %s, want the in-flight 429 to extend it", got)
		}
		if venue.count("candleSnapshot") != 1 || rlHostState(ledger, host).InFlight != 0 || ledger.snapshot().RateLimited != 2 {
			t.Fatalf("candle requests = %d, state = %+v; want the in-flight request finished and counted", venue.count("candleSnapshot"), rlHostState(ledger, host))
		}
	})

	t.Run("WebsocketOwnerStopsAndRecovers", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		host := feedHostOf(venue.srv.URL)
		ledger := newFeedRequestLedger(nil, false, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		req := rlRequirements(100, 60, "BTC", "ETH")
		reqs := fullCycleRequirements(req)
		btc, eth := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m")
		venue.setStatus("candleSnapshot", http.StatusTooManyRequests)
		<-owner.ApplyGeneration(ctx, req)
		sent := venue.count("candleSnapshot")
		if sent < 1 || sent > 2 {
			t.Fatalf("bootstrap sent %d candle request(s), want 1 or 2: only requests that began before the 429 may send", sent)
		}
		for _, key := range []marketFeedKey{btc, eth} {
			if r := rlKeyStatus(t, owner, key); r.Ready || r.Status != feedStatusRateLimited {
				t.Fatalf("%s = %+v, want rate_limited and not ready", key, r)
			}
		}
		events := ledger.rateLimitReport(feedRateLimitOutstanding(owner))
		if len(events) != 1 || events[0].Kind != feedRateLimitEventPaused || fmt.Sprint(events[0].Inputs) != "[BTC|1m ETH|1m]" {
			t.Fatalf("first report = %+v, want one pause alert naming [BTC|1m ETH|1m]", events)
		}

		clock.Advance(30 * time.Second)
		prepareMarketSnapshot(ctx, owner, reqs)
		if got := venue.count("candleSnapshot"); got != sent {
			t.Fatalf("cycle recovery during the pause sent %d request(s), want none", got-sent)
		}
		venue.setStatus("candleSnapshot", http.StatusOK)
		clock.Advance(feedRateLimitPause)
		if r := rlKeyStatus(t, owner, btc); r.Ready || r.Status != feedStatusRateLimited {
			t.Fatalf("after expiry with no fetch %s = %+v, want still rate_limited", btc, r)
		}
		if events := ledger.rateLimitReport(feedRateLimitOutstanding(owner)); len(events) != 0 {
			t.Fatalf("report after expiry with no fetch = %+v, want no alert", events)
		}
		clock.Advance(feedHistoryJitterSpan(feedHistoryRefusalFloor))
		runFeedHistoryPass(t, owner, ctx)
		for _, key := range []marketFeedKey{btc, eth} {
			if r := rlKeyStatus(t, owner, key); !r.Ready {
				t.Fatalf("after the recovery fetch %s = %+v, want ready", key, r)
			}
		}
		if got := venue.count("candleSnapshot") - sent; got != 2 {
			t.Fatalf("recovery candle requests = %d, want 2", got)
		}
		events = ledger.rateLimitReport(feedRateLimitOutstanding(owner))
		if len(events) != 1 || events[0].Kind != feedRateLimitEventRecover || events[0].Host != host {
			t.Fatalf("report after recovery = %+v, want one recovery alert", events)
		}
		for _, a := range owner.DrainAlerts() {
			switch a.Kind {
			case "bootstrap_failed", "repair_failed", "recovery_failed", "funding_failed", "accounting_funding_failed":
				t.Fatalf("owner raised per-key alert %+v for a rate limit", a)
			}
		}
	})

	t.Run("CorrectionKeepsOwnPacing", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		ledger := newFeedRequestLedger(nil, false, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		coins := make([]string, 0, feedCorrectionMaxPerMin+1)
		for i := 0; i <= feedCorrectionMaxPerMin; i++ {
			coins = append(coins, fmt.Sprintf("C%02d", i))
		}
		<-owner.ApplyGeneration(ctx, rlRequirements(100, 60, coins...))
		bootReads := venue.count("candleSnapshot")
		clock.Advance(5 * time.Second)
		venue.setStatus("allMids", http.StatusTooManyRequests)
		if _, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"}); !errors.Is(err, errHLInfoRateLimited) {
			t.Fatalf("mids err = %v, want errHLInfoRateLimited", err)
		}
		clock.Advance(25 * time.Second)
		owner.syncCorrectionPause(ledger)
		if jobs := owner.correctionPass(100); len(jobs) != 0 {
			t.Fatalf("correction pass during the ledger pause returned %d job(s), want 0", len(jobs))
		}
		owner.feedMu.Lock()
		window := len(owner.corrWindow)
		owner.feedMu.Unlock()
		if window != 0 || venue.count("candleSnapshot") != bootReads {
			t.Fatalf("corrWindow = %d, candle requests = %d; want no correction slot or request during the pause", window, venue.count("candleSnapshot")-bootReads)
		}
		for _, r := range owner.Readiness() {
			if !r.Ready {
				t.Fatalf("%s = %+v, want healthy keys to stay ready while correction waits", r.Key, r)
			}
		}
		clock.Advance(feedRateLimitPause)
		owner.syncCorrectionPause(ledger)
		jobs := owner.correctionPass(100)
		owner.feedMu.Lock()
		capped := owner.correction.Capped
		owner.feedMu.Unlock()
		if len(jobs) != feedCorrectionMaxPerMin || capped != 1 {
			t.Fatalf("correction pass after expiry = %d job(s), capped %d; want the cap of %d and one deferred key", len(jobs), capped, feedCorrectionMaxPerMin)
		}
		for _, job := range jobs {
			owner.runCorrectionJob(ctx, job)
		}
		if got := venue.count("candleSnapshot") - bootReads; got != feedCorrectionMaxPerMin {
			t.Fatalf("correction reads after expiry = %d, want %d", got, feedCorrectionMaxPerMin)
		}
	})

	restFlow := func(t *testing.T, enforce bool) {
		start := time.Unix(1890000000, 0).UTC()
		clock := &hlLotTestClock{now: start.Add(-time.Minute)}
		venue := newRLVenue(t, clock.Now)
		useHLInfoURLs(t, venue.srv.URL, venue.srv.URL)
		ledger := newFeedRequestLedger(&feedBudgetConfig{PerMinute: 100, Startup: 10}, enforce, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		owner := newMarketFeedOwner(clock.Now, nil)
		btc := feedKeyFor("BTC", "1m")
		req := rlRequirements(100, 180, "BTC")
		<-owner.ApplyGeneration(ctx, req)
		var budgetAlerts, rateAlerts []string
		var logs []string
		sealer := newFeedSealer(owner, feedSourceREST, "fixture", clock.Now, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
		sealer.ledger = ledger
		src := newFeedRESTSource(owner, nil)
		src.backoff = time.Millisecond
		sealer.prepareFn = src.prepare
		sealer.alertHook = func(msg string) { budgetAlerts = append(budgetAlerts, msg) }
		sealer.rateLimitSend = func(msg string) { rateAlerts = append(rateAlerts, msg) }
		sealer.setGeneration(req, []int{60}, start.Add(-time.Hour))
		seal := func(key int64, at time.Duration) *feedSeal {
			t.Helper()
			clock.mu.Lock()
			clock.now = time.Unix(key, 0).UTC().Add(at)
			clock.mu.Unlock()
			sealer.sealOne(ctx, key)
			sealer.mu.Lock()
			defer sealer.mu.Unlock()
			s := sealer.ring[key]
			if s == nil {
				t.Fatalf("no seal stored for key %d; logs %v", key, logs)
			}
			return s
		}
		k := start.Unix()

		venue.setStatus("candleSnapshot", http.StatusTooManyRequests)
		if s := seal(k, 5*time.Second); s.KeysReady != 0 {
			t.Fatalf("seal %d ready keys = %d, want 0 after a 429", k, s.KeysReady)
		}
		if venue.count("candleSnapshot") != 2 || venue.count("allMids") != 0 {
			t.Fatalf("requests = %d candle, %d mids; want one refresh 429 with no retry and mids refused", venue.count("candleSnapshot")-1, venue.count("allMids"))
		}
		if r := rlKeyStatus(t, owner, btc); r.Status != feedStatusRateLimited {
			t.Fatalf("key = %+v, want rate_limited", r)
		}
		venue.setStatus("candleSnapshot", http.StatusOK)

		if s := seal(k+60, 2*time.Second); s.KeysReady != 0 || venue.count("candleSnapshot") != 2 || venue.count("allMids") != 0 {
			t.Fatalf("paused seal: ready %d, candle %d, mids %d; want nothing sent", s.KeysReady, venue.count("candleSnapshot"), venue.count("allMids"))
		}
		venue.setStatus("allMids", http.StatusTooManyRequests)
		if s := seal(k+120, 5*time.Second); s.KeysReady != 0 {
			t.Fatalf("seal after expiry at a deadline where the key is not due: ready %d, want 0", s.KeysReady)
		}
		if venue.count("allMids") != 1 || venue.count("candleSnapshot") != 2 {
			t.Fatalf("requests = %d mids, %d candle; want one mids 429 that re-pauses the episode", venue.count("allMids"), venue.count("candleSnapshot"))
		}
		if st := rlHostState(ledger, feedHostOf(venue.srv.URL)); st.State != feedRateLimitStatePaused || st.Extensions != 1 {
			t.Fatalf("host state = %+v, want the same episode paused again with one extension", st)
		}
		venue.setStatus("allMids", http.StatusOK)
		if s := seal(k+180, 15*time.Second); s.KeysReady != 1 {
			t.Fatalf("due seal after the pause: ready %d, want 1", s.KeysReady)
		}
		if venue.count("candleSnapshot") != 3 || venue.count("allMids") != 2 {
			t.Fatalf("requests = %d candle, %d mids; want one each after the pause", venue.count("candleSnapshot"), venue.count("allMids"))
		}
		if len(rateAlerts) != 2 || len(budgetAlerts) != 0 {
			t.Fatalf("rate alerts %q, budget alerts %q; want one pause and one recovery alert and no budget alert", rateAlerts, budgetAlerts)
		}
		want := fmt.Sprintf("**MARKET FEED RATE LIMITED** [rest] host %s answered HTTP 429; every feed read to this host waits until %s. Refused inputs: BTC|1m,mids.",
			feedHostOf(venue.srv.URL), time.Unix(k, 0).UTC().Add(5*time.Second+feedRateLimitPause).Format(time.RFC3339))
		if rateAlerts[0][:len(want)] != want {
			t.Fatalf("pause alert = %q, want prefix %q", rateAlerts[0], want)
		}
		recovered := "**MARKET FEED RATE LIMIT RECOVERED** [rest] host " + feedHostOf(venue.srv.URL)
		if rateAlerts[1][:len(recovered)] != recovered {
			t.Fatalf("second alert = %q, want the recovery alert", rateAlerts[1])
		}
	}

	t.Run("RESTOwnerStopsAndRecovers", func(t *testing.T) { restFlow(t, true) })
	t.Run("EpisodeAlerts", func(t *testing.T) { restFlow(t, false) })
}
