package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func useHLLotMetadataForTest(t *testing.T, coins map[string]int) *hlLotMetadataCache {
	t.Helper()
	prev := hlLotMetadata
	c := newHLLotMetadataCache(nil)
	c.logf = func(string, ...any) {}
	c.fetch = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("test metadata cache has no network")
	}
	if coins != nil {
		entries := make(map[string]hlLotEntry, len(coins))
		for coin, sz := range coins {
			entries[coin] = hlLotEntry{SzDecimals: sz}
		}
		endpoint := c.endpoint()
		c.snaps[endpoint] = &hlLotSnapshot{Endpoint: endpoint, FetchedAt: c.clock(), Coins: entries}
	}
	hlLotMetadata = c
	t.Cleanup(func() { hlLotMetadata = prev })
	return c
}

type hlMetaServer struct {
	srv      *httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	status   int
	body     string
	gate     chan struct{}
	entered  chan struct{}
}

func newHLMetaServer(t *testing.T, body string) *hlMetaServer {
	t.Helper()
	m := &hlMetaServer{status: http.StatusOK, body: body}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/info" || req["type"] != "meta" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		m.requests.Add(1)
		m.mu.Lock()
		gate, entered := m.gate, m.entered
		status, out := m.status, m.body
		m.mu.Unlock()
		if entered != nil {
			entered <- struct{}{}
		}
		if gate != nil {
			<-gate
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *hlMetaServer) set(status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.body = status, body
}

func hlMetaBody(entries ...string) string {
	return `{"universe":[` + strings.Join(entries, ",") + `],"marginTables":[]}`
}

type hlLotTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *hlLotTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *hlLotTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newHLLotTestCache(t *testing.T, ctx context.Context) (*hlLotMetadataCache, *hlLotTestClock, *[]string) {
	t.Helper()
	clock := &hlLotTestClock{now: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	c := newHLLotMetadataCache(ctx)
	c.clock = clock.Now
	var lines []string
	var linesMu sync.Mutex
	c.logf = func(format string, a ...any) {
		linesMu.Lock()
		defer linesMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, a...))
	}
	return c, clock, &lines
}

func useHLInfoURLs(t *testing.T, mainnet, testnet string) {
	t.Helper()
	prevMain, prevTest := hlMainnetURL, hlTestnetURL
	hlMainnetURL, hlTestnetURL = mainnet, testnet
	t.Cleanup(func() { hlMainnetURL, hlTestnetURL = prevMain, prevTest })
	t.Setenv("HYPERLIQUID_TESTNET", "")
}

func TestHLLotMetadataFetchesThroughTheBudgetedTransport(t *testing.T) {
	mainSrv := newHLMetaServer(t, hlMetaBody(`{"name":"ETH","szDecimals":4}`, `{"name":"DOGE","szDecimals":0}`, `{"name":"BTC","szDecimals":5}`))
	testSrv := newHLMetaServer(t, hlMetaBody(`{"name":"ETH","szDecimals":3}`))
	useHLInfoURLs(t, mainSrv.srv.URL, testSrv.srv.URL)
	ledger := newFeedRequestLedger(nil, false, nil)
	c, clock, lines := newHLLotTestCache(t, withFeedLedger(context.Background(), ledger))

	if got := c.Lookup("ETH"); got.Known || !strings.Contains(got.Reason, "no venue metadata fetched yet") {
		t.Fatalf("cold lookup = %+v, want unknown before any fetch", got)
	}
	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 1 {
		t.Fatalf("mainnet requests = %d, want 1", n)
	}
	totals := ledger.snapshot()
	if totals.ByReason[string(feedRestLotMeta)] != 1 || totals.ByType["meta"] != 1 {
		t.Fatalf("ledger totals = %+v, want one meta request charged as %s", totals, feedRestLotMeta)
	}
	for coin, want := range map[string]int{"ETH": 4, "DOGE": 0, "BTC": 5} {
		if got := c.Lookup(coin); !got.Known || got.SzDecimals != want {
			t.Fatalf("Lookup(%s) = %+v, want szDecimals %d", coin, got, want)
		}
	}
	for _, coin := range []string{"eth", "ETH-PERP", "SOL"} {
		if got := c.Lookup(coin); got.Known || got.Reason != "coin absent from the venue perps universe" {
			t.Fatalf("Lookup(%s) = %+v, want an exact-coin miss", coin, got)
		}
	}

	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 1 {
		t.Fatalf("fresh snapshot refetched: requests = %d", n)
	}

	t.Setenv("HYPERLIQUID_TESTNET", "1")
	if got := c.Lookup("ETH"); got.Known {
		t.Fatalf("testnet lookup used the mainnet snapshot: %+v", got)
	}
	c.Ensure("ETH")
	if got := c.Lookup("ETH"); !got.Known || got.SzDecimals != 3 || got.Endpoint != testSrv.srv.URL {
		t.Fatalf("testnet lookup = %+v, want szDecimals 3 from the testnet endpoint", got)
	}
	t.Setenv("HYPERLIQUID_TESTNET", "")
	if got := c.Lookup("ETH"); !got.Known || got.SzDecimals != 4 {
		t.Fatalf("mainnet lookup after the testnet fetch = %+v, want 4", got)
	}

	clock.Advance(hlLotMetadataRefreshAfter + time.Minute)
	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 2 {
		t.Fatalf("requests after the refresh age = %d, want 2", n)
	}

	mainSrv.set(http.StatusInternalServerError, "down")
	clock.Advance(hlLotMetadataExpiry)
	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 3 {
		t.Fatalf("requests after expiry = %d, want 3", n)
	}
	*lines = nil
	for i := 0; i < 3; i++ {
		if got := c.Lookup("ETH"); got.Known || !strings.Contains(got.Reason, "venue metadata expired") || !strings.Contains(got.Reason, "http 500") {
			t.Fatalf("expired lookup = %+v, want an expired reason that names the failed refresh", got)
		}
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "ETH lot size unavailable") {
		t.Fatalf("outage lines = %q, want exactly one", *lines)
	}

	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 3 {
		t.Fatalf("retry before the backoff elapsed: requests = %d", n)
	}
	clock.Advance(hlLotMetadataRetryInitial)
	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 4 {
		t.Fatalf("requests after the first backoff = %d, want 4", n)
	}
	clock.Advance(hlLotMetadataRetryInitial)
	c.Ensure("ETH")
	if n := mainSrv.requests.Load(); n != 4 {
		t.Fatalf("second backoff did not double: requests = %d", n)
	}
	for i := 0; i < 12; i++ {
		clock.Advance(hlLotMetadataRetryMax)
		c.Ensure("ETH")
	}
	if wait := c.retryWait[mainSrv.srv.URL]; wait != hlLotMetadataRetryMax {
		t.Fatalf("retry wait = %s, want capped at %s", wait, hlLotMetadataRetryMax)
	}

	mainSrv.set(http.StatusOK, hlMetaBody(`{"name":"ETH","szDecimals":4}`))
	clock.Advance(hlLotMetadataRetryMax)
	c.Ensure("ETH")
	*lines = nil
	if got := c.Lookup("ETH"); !got.Known || got.SzDecimals != 4 {
		t.Fatalf("lookup after recovery = %+v", got)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "restored") {
		t.Fatalf("recovery lines = %q, want one restored line", *lines)
	}
	c.Lookup("ETH")
	if len(*lines) != 1 {
		t.Fatalf("a known lookup logged again: %q", *lines)
	}

	mainSrv.set(http.StatusOK, hlMetaBody(`{"name":"BTC","szDecimals":5}`))
	clock.Advance(hlLotMetadataRefreshAfter + time.Minute)
	c.Ensure("ETH")
	*lines = nil
	c.Lookup("ETH")
	c.Lookup("ETH")
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "coin absent") {
		t.Fatalf("a new outage after recovery logged %q, want one new line", *lines)
	}
}

func TestHLLotMetadataRefreshIsSharedAndRunsOutsideTheSchedulerLock(t *testing.T) {
	srv := newHLMetaServer(t, hlMetaBody(`{"name":"ETH","szDecimals":4}`))
	useHLInfoURLs(t, srv.srv.URL, "http://127.0.0.1:1")
	c, _, _ := newHLLotTestCache(t, nil)
	srv.mu.Lock()
	srv.gate = make(chan struct{})
	srv.entered = make(chan struct{}, 16)
	srv.mu.Unlock()

	var schedulerMu sync.RWMutex
	schedulerMu.Lock()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Ensure("ETH")
		}()
	}
	select {
	case <-srv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh reached the venue")
	}
	time.Sleep(50 * time.Millisecond)
	close(srv.gate)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh waited on the scheduler lock")
	}
	schedulerMu.Unlock()
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("concurrent refreshes sent %d requests, want 1", n)
	}
	if got := c.Lookup("ETH"); !got.Known || got.SzDecimals != 4 {
		t.Fatalf("lookup after the shared refresh = %+v", got)
	}
}

func TestHLLotMetadataLocalLedgerBoundsRequests(t *testing.T) {
	srv := newHLMetaServer(t, "not json")
	useHLInfoURLs(t, srv.srv.URL, "http://127.0.0.1:1")
	c, clock, _ := newHLLotTestCache(t, nil)
	ledger := feedLedgerFrom(c.baseCtx)
	if ledger == nil || !ledger.enforce || ledger.perMinute != hlLotMetadataLedgerPerMinute {
		t.Fatalf("standalone cache ledger = %+v, want an enforced local ledger", ledger)
	}
	for i := 0; i < hlLotMetadataLedgerPerMinute+2; i++ {
		c.Ensure("ETH")
		clock.Advance(hlLotMetadataRetryMax)
	}
	if n := srv.requests.Load(); n != hlLotMetadataLedgerPerMinute {
		t.Fatalf("requests = %d, want the %d-per-minute local budget", n, hlLotMetadataLedgerPerMinute)
	}
	if got := ledger.status(); got.Totals.Refused != 2 {
		t.Fatalf("refused = %d, want 2", got.Totals.Refused)
	}
	if !strings.Contains(c.lastErr[srv.srv.URL], errFeedBudgetExhausted.Error()) {
		t.Fatalf("last refresh error = %q, want the budget refusal", c.lastErr[srv.srv.URL])
	}
	if got := c.Lookup("ETH"); got.Known {
		t.Fatalf("malformed response produced a lot size: %+v", got)
	}
}

func TestHLLotMetadataParsesOnlyValidLotSizes(t *testing.T) {
	coins, err := parseHLLotMetadata([]byte(hlMetaBody(
		`{"name":"ZERO","szDecimals":0}`,
		`{"name":"OK","szDecimals":5}`,
		`{"name":"ABSENT"}`,
		`{"name":"NULL","szDecimals":null}`,
		`{"name":"BOOL","szDecimals":true}`,
		`{"name":"FRAC","szDecimals":4.5}`,
		`{"name":"FLOAT","szDecimals":4.0}`,
		`{"name":"NEG","szDecimals":-1}`,
		`{"name":"STR","szDecimals":"4"}`,
		`{"name":"BIG","szDecimals":13}`,
		`{"name":"DUP","szDecimals":2}`,
		`{"name":"DUP","szDecimals":2}`,
		`{"szDecimals":3}`,
	)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e := coins["ZERO"]; e.Problem != "" || e.SzDecimals != 0 {
		t.Fatalf("valid zero = %+v", e)
	}
	if e := coins["OK"]; e.Problem != "" || e.SzDecimals != 5 {
		t.Fatalf("valid five = %+v", e)
	}
	for _, name := range []string{"ABSENT", "NULL", "BOOL", "FRAC", "FLOAT", "NEG", "STR", "BIG", "DUP"} {
		if e, ok := coins[name]; !ok || e.Problem == "" {
			t.Fatalf("%s = %+v (present %v), want a refused entry", name, e, ok)
		}
	}
	if len(coins) != 11 {
		t.Fatalf("parsed %d coins, want 11 named entries", len(coins))
	}
	for name, body := range map[string]string{
		"not json":       "{",
		"no universe":    `{"marginTables":[]}`,
		"empty universe": `{"universe":[]}`,
		"null universe":  `{"universe":null}`,
		"wrong shape":    `{"universe":{"ETH":4}}`,
	} {
		if _, err := parseHLLotMetadata([]byte(body)); err == nil {
			t.Fatalf("%s parsed without error", name)
		}
	}

	big := newHLMetaServer(t, `{"universe":[`+strings.Repeat(" ", hlLotMetadataMaxResponseBytes)+`]}`)
	if _, err := fetchHLLotMetadataRaw(context.Background(), big.srv.URL); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response err = %v, want a size refusal", err)
	}
}

func TestHLFloorLotSizeMatchesThePythonDecimalFloor(t *testing.T) {
	cases := []struct {
		qty  float64
		d    int
		want float64
	}{
		{0.420714573875666, 4, 0.4207},
		{0.1682858295502664, 4, 0.1682},
		{0.25242874432539963, 4, 0.2524},
		{0.29999999999, 4, 0.2999},
		{0.29999999999999, 4, 0.3},
		{2.9999999999, 0, 3.0},
		{2.99999999999, 0, 3.0},
		{3999.2001, 0, 3999.0},
		{1e-05, 4, 0},
		{0.00009999999999999, 4, 0.0001},
		{123456.78901234567, 2, 123456.78},
		{0.5, 5, 0.5},
		{7.0, 0, 7.0},
		{1.23456789e-07, 5, 0},
		{0, 4, 0},
		{-1, 4, 0},
		{math.NaN(), 4, 0},
		{math.Inf(1), 4, 0},
		{1.25, -2, 1},
	}
	for _, c := range cases {
		if got := hlFloorLotSize(c.qty, c.d); got != c.want {
			t.Errorf("hlFloorLotSize(%v, %d) = %v, want %v", c.qty, c.d, got, c.want)
		}
	}
}

func TestHLVenueDecisionsUseTheLiveMinimumBoundary(t *testing.T) {
	lot4 := hlLotLookup{Known: true, SzDecimals: 4}
	lot0 := hlLotLookup{Known: true, SzDecimals: 0}
	if th := hlVenueCloseGateThresholdUSD(); th != 10.3 {
		t.Fatalf("threshold = %v, want 10.3", th)
	}
	cases := []struct {
		name string
		got  hlPaperLotDecision
		qty  float64
		hold string
	}{
		{"entry equal to the minimum passes", hlVenueEntryDecision(1.00009, 10.3, lot0), 1, ""},
		{"entry below the minimum holds", hlVenueEntryDecision(1.9, 10.29, lot0), 1, hlPaperHoldBelowMin},
		{"entry at the minimum after the floor passes", hlVenueEntryDecision(0.0103019, 1000, lot4), 0.0103, ""},
		{"entry flooring to zero holds", hlVenueEntryDecision(0.99, 50, lot0), 0, hlPaperHoldBelowLot},
		{"entry with an unknown lot holds", hlVenueEntryDecision(1, 100, hlLotLookup{Reason: "x"}), 0, hlPaperHoldLotUnknown},
		{"entry with a bad price holds", hlVenueEntryDecision(1, math.NaN(), lot0), 0, hlPaperHoldInvalidPx},
		{"entry with a bad quantity holds", hlVenueEntryDecision(math.Inf(1), 10, lot0), 0, hlPaperHoldInvalidQty},
		{"close equal to the minimum passes", hlVenuePartialCloseDecision(1.5, 10.3, lot0), 1, ""},
		{"close one lot above the minimum passes", hlVenuePartialCloseDecision(0.10319, 100, lot4), 0.1031, ""},
		{"close below the minimum holds", hlVenuePartialCloseDecision(0.10299, 100, lot4), 0.1029, hlPaperHoldBelowMin},
		{"close flooring to zero holds", hlVenuePartialCloseDecision(0.00009, 1e6, lot4), 0, hlPaperHoldBelowLot},
		{"close with an unknown lot holds", hlVenuePartialCloseDecision(1, 100, hlLotLookup{Reason: "x"}), 0, hlPaperHoldLotUnknown},
		{"close with a zero decision price holds", hlVenuePartialCloseDecision(1, 0, lot0), 0, hlPaperHoldInvalidPx},
	}
	for _, c := range cases {
		if c.got.Hold != c.hold || c.got.Qty != c.qty {
			t.Errorf("%s: decision = %+v, want qty %v hold %q", c.name, c.got, c.qty, c.hold)
		}
	}
}

type paperLotFixture struct {
	name string
	cfg  *Config
	sc   StrategyConfig
	part RiskPartition
}

func paperLotFixtures(t *testing.T, coin string) []paperLotFixture {
	t.Helper()
	base := func(id string) StrategyConfig {
		return StrategyConfig{
			ID: id, Type: "perps", Platform: "hyperliquid", Script: "shared_scripts/check_hyperliquid.py",
			Args:      []string{"sma", coin, "1h", "--mode=paper"},
			Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1, Capital: 1000, AllowScaleIn: true,
		}
	}
	dir := t.TempDir()
	combined := base("hl-lot-combined")
	split := base("hl-lot-split")
	sourced := base("hl-lot-source")
	sourced.PaperSource = "lot"
	sourced.StorageStrategyID = "hl-lot-alias"
	return []paperLotFixture{
		{"combined storage", &Config{DBFile: filepath.Join(dir, "combined.db"), Strategies: []StrategyConfig{combined}}, combined, defaultPaperPartition},
		{"split paper storage", &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"), Strategies: []StrategyConfig{split}}, split, defaultPaperPartition},
		{"named paper source with a storage alias", &Config{DBFile: filepath.Join(dir, "live2.db"), PaperSources: []PaperSourceConfig{{ID: "lot", DBFile: filepath.Join(dir, "lot.db")}}, Strategies: []StrategyConfig{sourced}}, sourced, paperSourcePartition("lot")},
	}
}

func lotAligned(qty float64, d int) bool {
	scaled := qty * math.Pow(10, float64(d))
	return math.Abs(scaled-math.Round(scaled)) < 1e-6
}

func strategyStateJSON(t *testing.T, s *StrategyState) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return string(b)
}

func paperDispatch(t *testing.T, sc StrategyConfig, s *StrategyState, result *HyperliquidResult, mid float64) int {
	t.Helper()
	trades, _, _, _ := executeHyperliquidResultDeferredOpen(sc, s, result, nil, "SIGNAL", mid, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
	return trades
}

func requireHeld(t *testing.T, what string, sc StrategyConfig, s *StrategyState, run func() int) {
	t.Helper()
	before := strategyStateJSON(t, s)
	if n := run(); n != 0 {
		t.Fatalf("%s booked %d trades, want a hold", what, n)
	}
	if after := strategyStateJSON(t, s); after != before {
		t.Fatalf("%s changed the book:\nbefore %s\nafter  %s", what, before, after)
	}
}

func hlLotTestResult(coin string, signal int, price, closeFraction, tierPx float64) *HyperliquidResult {
	r := &HyperliquidResult{Symbol: coin, Signal: signal, Price: price}
	r.CloseFraction = closeFraction
	r.CloseTierFillPrice = tierPx
	return r
}

func lastTrade(t *testing.T, s *StrategyState) Trade {
	t.Helper()
	if len(s.TradeHistory) == 0 {
		t.Fatal("no trade recorded")
	}
	return s.TradeHistory[len(s.TradeHistory)-1]
}

func TestPaperHLSyntheticOrdersBookVenueLotsAndPersist(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	specs := []struct {
		coin string
		d    int
		px   float64
	}{
		{"ETH", 4, 2000.123},
		{"DOGE", 0, 0.2537},
	}
	for _, spec := range specs {
		for _, side := range []string{"long", "short"} {
			for _, fx := range paperLotFixtures(t, spec.coin) {
				t.Run(fmt.Sprintf("%s szDecimals %d %s %s", spec.coin, spec.d, fx.name, side), func(t *testing.T) {
					useHLLotMetadataForTest(t, map[string]int{spec.coin: spec.d})
					runPaperLotScenario(t, fx, spec.coin, spec.d, spec.px, side)
				})
			}
		}
	}
}

func runPaperLotScenario(t *testing.T, fx paperLotFixture, coin string, d int, px float64, side string) {
	sc := fx.sc
	openSig, closeSig := 1, -1
	if side == "short" {
		openSig, closeSig = -1, 1
	}
	pnlOf := func(qty, avg, exit float64) float64 {
		if side == "long" {
			return qty * (exit - avg)
		}
		return qty * (avg - exit)
	}
	state := NewAppState()
	s := &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	state.Strategies[sc.ID] = s

	if n := paperDispatch(t, sc, s, &HyperliquidResult{Symbol: coin, Signal: openSig, Price: px}, px); n != 1 {
		t.Fatalf("open trades = %d", n)
	}
	open := lastTrade(t, s)
	sizing := PerpsSizingFor(sc, px, 0)
	requested := PerpsOpenNotionalSized(1000, open.Price, sizing) / open.Price
	wantQty := hlFloorLotSize(requested, d)
	pos := s.Positions[coin]
	if pos == nil || pos.Side != side || pos.Quantity != wantQty || pos.InitialQuantity != wantQty || open.Quantity != wantQty || !lotAligned(wantQty, d) {
		t.Fatalf("open = %+v trade %+v, want %s qty %v (requested %v)", pos, open, side, wantQty, requested)
	}
	if wantQty == requested {
		t.Fatalf("fixture requested an already aligned quantity %v; it proves nothing", requested)
	}
	openFee := CalculatePlatformSpotFee("hyperliquid", wantQty*open.Price)
	if !approxEq(open.ExchangeFee, openFee) || !approxEq(s.Cash, 1000-openFee) {
		t.Fatalf("open fee %v cash %v, want fee %v cash %v", open.ExchangeFee, s.Cash, openFee, 1000-openFee)
	}
	avg := pos.AvgCost

	cash := s.Cash
	exitPx := px * 1.02
	if n := paperDispatch(t, sc, s, hlLotTestResult(coin, closeSig, exitPx, 0.3721, 0), exitPx); n != 1 {
		t.Fatalf("partial close trades = %d", n)
	}
	pc := lastTrade(t, s)
	wantClose := hlFloorLotSize(wantQty*0.3721, d)
	if !pc.IsClose || pc.Quantity != wantClose || !lotAligned(wantClose, d) || wantClose == wantQty*0.3721 {
		t.Fatalf("partial close trade %+v, want qty %v from %v", pc, wantClose, wantQty*0.3721)
	}
	pcFee := CalculatePlatformSpotFee("hyperliquid", wantClose*pc.Price)
	if !approxEq(pc.RealizedPnL, pnlOf(wantClose, avg, pc.Price)) || !approxEq(pc.ExchangeFee, pcFee) || !approxEq(s.Cash, cash+pnlOf(wantClose, avg, pc.Price)-pcFee) {
		t.Fatalf("partial close pnl %v fee %v cash %v", pc.RealizedPnL, pc.ExchangeFee, s.Cash)
	}
	remaining := wantQty - wantClose
	if pos = s.Positions[coin]; pos == nil || !approxEq(pos.Quantity, remaining) || pos.InitialQuantity != wantQty {
		t.Fatalf("after the partial close %+v, want qty %v and initial %v", pos, remaining, wantQty)
	}
	remaining = pos.Quantity

	tierPx := px * 1.05
	if side == "short" {
		tierPx = px * 0.95
	}
	cash = s.Cash
	tierRes := hlLotTestResult(coin, closeSig, px, 0.5, tierPx)
	if n := paperDispatch(t, sc, s, tierRes, px); n != 1 {
		t.Fatalf("tier fill trades = %d", n)
	}
	tier := lastTrade(t, s)
	wantTier := hlFloorLotSize(remaining*0.5, d)
	if tier.Quantity != wantTier || tier.Price != tierPx || !lotAligned(wantTier, d) {
		t.Fatalf("tier fill %+v, want qty %v @ %v", tier, wantTier, tierPx)
	}
	tierFee := CalculatePlatformSpotFee("hyperliquid", wantTier*tierPx)
	if !approxEq(s.Cash, cash+pnlOf(wantTier, avg, tierPx)-tierFee) {
		t.Fatalf("tier fill cash %v, want %v", s.Cash, cash+pnlOf(wantTier, avg, tierPx)-tierFee)
	}
	remaining = pos.Quantity

	requireHeld(t, "partial close that floors to zero", sc, s, func() int {
		return paperDispatch(t, sc, s, hlLotTestResult(coin, closeSig, px, math.Pow(10, -float64(d))/remaining/2, 0), px)
	})
	requireHeld(t, "partial close below the venue minimum", sc, s, func() int {
		return paperDispatch(t, sc, s, hlLotTestResult(coin, closeSig, px, (5/px)/remaining, 0), px)
	})
	requireHeld(t, "tier fill below the venue minimum", sc, s, func() int {
		return paperDispatch(t, sc, s, hlLotTestResult(coin, closeSig, px, (5/px)/remaining, tierPx), px)
	})

	cash = s.Cash
	addReq := 37.123 / px
	addRes := &HyperliquidResult{Symbol: coin, Signal: openSig, Price: px}
	n, _, _, _ := executeHyperliquidScaleInDeferredOpen(sc, s, addRes, nil, "ADD", px, addReq, silentStrategyLogger(sc.ID))
	add := lastTrade(t, s)
	wantAdd := hlFloorLotSize(addReq, d)
	if n != 1 || add.TradeType != scaleInTradeType || add.Quantity != wantAdd || !lotAligned(wantAdd, d) || wantAdd == addReq {
		t.Fatalf("scale-in = %d trade %+v, want add %v from %v", n, add, wantAdd, addReq)
	}
	if pos = s.Positions[coin]; !approxEq(pos.Quantity, remaining+wantAdd) || pos.InitialQuantity != wantQty+wantAdd || pos.ScaleInCount != 1 {
		t.Fatalf("after the add %+v, want qty %v initial %v", pos, remaining+wantAdd, wantQty+wantAdd)
	}
	addPx := px * (1 + SlippagePct)
	if side == "short" {
		addPx = px * (1 - SlippagePct)
	}
	if math.Abs(add.Price-addPx) > 1e-9 || !approxEq(s.Cash, cash-CalculatePlatformSpotFee("hyperliquid", wantAdd*addPx)) {
		t.Fatalf("scale-in cash %v price %v, want the add at %v (mid moved against the add side by SlippagePct)", s.Cash, add.Price, addPx)
	}
	requireHeld(t, "scale-in below the venue minimum", sc, s, func() int {
		n, _, _, _ := executeHyperliquidScaleInDeferredOpen(sc, s, addRes, nil, "ADD", px, 5/px, silentStrategyLogger(sc.ID))
		return n
	})
	requireHeld(t, "scale-in that floors to zero", sc, s, func() int {
		n, _, _, _ := executeHyperliquidScaleInDeferredOpen(sc, s, addRes, nil, "ADD", px, math.Pow(10, -float64(d))/2, silentStrategyLogger(sc.ID))
		return n
	})

	flipPx := px * 0.99
	closedBefore := len(s.ClosedPositions)
	heldQty := s.Positions[coin].Quantity
	if n := paperDispatch(t, sc, s, &HyperliquidResult{Symbol: coin, Signal: closeSig, Price: flipPx}, flipPx); n != 2 {
		t.Fatalf("flip trades = %d, want close and open", n)
	}
	trades := s.TradeHistory
	flipClose, flipOpen := trades[len(trades)-2], trades[len(trades)-1]
	if !flipClose.IsClose || !approxEq(flipClose.Quantity, heldQty) || len(s.ClosedPositions) != closedBefore+1 {
		t.Fatalf("flip close %+v, want the whole %v book closed", flipClose, heldQty)
	}
	newSide := "short"
	if side == "short" {
		newSide = "long"
	}
	flipReq := PerpsOpenNotionalSized(s.Cash+CalculatePlatformSpotFee("hyperliquid", flipOpen.Quantity*flipOpen.Price), flipOpen.Price, sizing) / flipOpen.Price
	if pos = s.Positions[coin]; pos == nil || pos.Side != newSide || pos.Quantity != hlFloorLotSize(flipReq, d) || !lotAligned(pos.Quantity, d) {
		t.Fatalf("flip opened %+v, want %s qty %v", pos, newSide, hlFloorLotSize(flipReq, d))
	}

	store := openSourceStore(t, fx.cfg)
	for part, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}
	reloaded, _, err := LoadStateWithStore(fx.cfg, store)
	if err != nil {
		t.Fatalf("LoadStateWithStore: %v", err)
	}
	rs := reloaded.Strategies[sc.ID]
	if rs == nil {
		t.Fatal("strategy missing after reload")
	}
	rp := rs.Positions[coin]
	if rp == nil || rp.Quantity != pos.Quantity || rp.InitialQuantity != pos.InitialQuantity || rp.Side != pos.Side || !approxEq(rs.Cash, s.Cash) {
		t.Fatalf("reloaded book %+v cash %v, want %+v cash %v", rp, rs.Cash, pos, s.Cash)
	}
	if len(rs.TradeHistory) != len(s.TradeHistory) {
		t.Fatalf("reloaded %d trades, want %d", len(rs.TradeHistory), len(s.TradeHistory))
	}
	booked := make([]float64, 0, len(s.TradeHistory))
	for _, tr := range s.TradeHistory {
		booked = append(booked, tr.Quantity)
	}
	got := make([]float64, 0, len(rs.TradeHistory))
	for _, tr := range rs.TradeHistory {
		got = append(got, tr.Quantity)
	}
	sort.Float64s(booked)
	sort.Float64s(got)
	for i := range booked {
		if got[i] != booked[i] || !lotAligned(got[i], d) {
			t.Fatalf("reloaded trade quantities %v, want lot-aligned %v", got, booked)
		}
	}
}

func TestPaperHLMetadataOutageHoldsEntriesAndPartialsButNotFullCloses(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	legacyQty := 0.420714573875666
	outages := []struct {
		name  string
		setup func(t *testing.T) *hlLotMetadataCache
	}{
		{"cold start", func(t *testing.T) *hlLotMetadataCache { return useHLLotMetadataForTest(t, nil) }},
		{"coin absent", func(t *testing.T) *hlLotMetadataCache { return useHLLotMetadataForTest(t, map[string]int{"BTC": 5}) }},
		{"expired", func(t *testing.T) *hlLotMetadataCache {
			c := useHLLotMetadataForTest(t, map[string]int{"ETH": 4})
			now := c.clock()
			c.clock = func() time.Time { return now.Add(hlLotMetadataExpiry) }
			return c
		}},
		{"malformed entry", func(t *testing.T) *hlLotMetadataCache {
			c := useHLLotMetadataForTest(t, nil)
			coins, err := parseHLLotMetadata([]byte(hlMetaBody(`{"name":"ETH","szDecimals":null}`)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			c.snaps[c.endpoint()] = &hlLotSnapshot{Endpoint: c.endpoint(), FetchedAt: c.clock(), Coins: coins}
			return c
		}},
	}
	for _, o := range outages {
		for _, fx := range paperLotFixtures(t, "ETH") {
			t.Run(o.name+" "+fx.name, func(t *testing.T) {
				cache := o.setup(t)
				var lines []string
				cache.logf = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
				sc := fx.sc
				newState := func(pos *Position) (*AppState, *StrategyState) {
					state := NewAppState()
					s := &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
					if pos != nil {
						s.Positions["ETH"] = pos
					}
					state.Strategies[sc.ID] = s
					return state, s
				}
				legacy := func(side string) *Position {
					return &Position{Symbol: "ETH", Side: side, Quantity: legacyQty, InitialQuantity: legacyQty, AvgCost: 2000, OwnerStrategyID: sc.ID, Multiplier: 1, Leverage: 1}
				}

				_, flat := newState(nil)
				requireHeld(t, "open long", sc, flat, func() int {
					return paperDispatch(t, sc, flat, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, 2000)
				})
				requireHeld(t, "open short", sc, flat, func() int {
					return paperDispatch(t, sc, flat, &HyperliquidResult{Symbol: "ETH", Signal: -1, Price: 2000}, 2000)
				})
				_, held := newState(legacy("long"))
				requireHeld(t, "partial close", sc, held, func() int {
					return paperDispatch(t, sc, held, hlLotTestResult("ETH", -1, 2000, 0.4, 0), 2000)
				})
				requireHeld(t, "tier fill", sc, held, func() int {
					return paperDispatch(t, sc, held, hlLotTestResult("ETH", -1, 2000, 0.4, 2050), 2000)
				})
				requireHeld(t, "scale-in add", sc, held, func() int {
					n, _, _, _ := executeHyperliquidScaleInDeferredOpen(sc, held, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, nil, "ADD", 2000, 0.05, silentStrategyLogger(sc.ID))
					return n
				})
				if len(lines) != 1 || !strings.Contains(lines[0], "ETH lot size unavailable") {
					t.Fatalf("outage lines = %q, want exactly one", lines)
				}

				_, full := newState(legacy("long"))
				if n := paperDispatch(t, sc, full, hlLotTestResult("ETH", -1, 2000, 1, 0), 2000); n != 1 || full.Positions["ETH"] != nil || lastTrade(t, full).Quantity != legacyQty {
					t.Fatalf("full signal close = %d trades, book %+v", n, full.Positions["ETH"])
				}
				_, flip := newState(legacy("short"))
				if n := paperDispatch(t, sc, flip, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, 2000); n != 1 || flip.Positions["ETH"] != nil || lastTrade(t, flip).Quantity != legacyQty {
					t.Fatalf("flip during the outage = %d trades, book %+v; want the full close booked and the open held", n, flip.Positions["ETH"])
				}
				_, stopped := newState(legacy("long"))
				if !recordPerpsStopLossClose(stopped, "ETH", 1900, paperStopReasonPct, silentStrategyLogger(sc.ID)) || stopped.Positions["ETH"] != nil || lastTrade(t, stopped).Quantity != legacyQty {
					t.Fatalf("stop close during the outage left %+v", stopped.Positions["ETH"])
				}
				state, flattened := newState(legacy("short"))
				if closed := forceClosePaperScopePositions(state, fx.cfg, fx.part, map[string]float64{"ETH": 2100}); len(closed) != 1 || flattened.Positions["ETH"] != nil || lastTrade(t, flattened).Quantity != legacyQty {
					t.Fatalf("kill-switch flatten during the outage closed %v, left %+v", closed, flattened.Positions["ETH"])
				}
			})
		}
	}
}

func TestConfirmedAndReplayFillsKeepTheirQuantities(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	useHLLotMetadataForTest(t, map[string]int{"ETH": 4})
	unaligned := 0.123456789

	live := StrategyConfig{ID: "hl-lot-live", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=live"}, Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
	s := &StrategyState{ID: live.ID, Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{}}
	exec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: unaligned, OID: 9}}}
	if n, _, _, _ := executeHyperliquidResultDeferredOpen(live, s, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, exec, "BUY", 2000, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(live.ID)); n != 1 || s.Positions["ETH"].Quantity != unaligned {
		t.Fatalf("live fill booked %+v, want the confirmed %v", s.Positions["ETH"], unaligned)
	}
	addExec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.00012, OID: 10}}}
	if n, _, _, _ := executeHyperliquidScaleInDeferredOpen(live, s, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, addExec, "ADD", 2000, 0.00012, silentStrategyLogger(live.ID)); n != 1 || !approxEq(s.Positions["ETH"].Quantity, unaligned+0.00012) {
		t.Fatalf("live scale-in booked %+v, want the confirmed sub-minimum add", s.Positions["ETH"])
	}

	paper := StrategyConfig{ID: "hl-lot-mirror", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
	ps := &StrategyState{ID: paper.ID, Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{}}
	row := ReplayDecision{StrategyID: paper.ID, DecisionType: ReplayDecisionOpen, DecidedAt: time.Now().UTC(), Symbol: "ETH", Side: "long", Quantity: unaligned, ReferencePrice: 2000}
	if n, _ := replayBookOpen(paper, ps, row, nil, &Config{}, silentStrategyLogger(paper.ID)); n != 1 || ps.Positions["ETH"].Quantity != unaligned {
		t.Fatalf("replay open booked %+v, want the recorded %v", ps.Positions["ETH"], unaligned)
	}
}

func lotTierStrategy() StrategyConfig {
	slMult := 1.0
	return StrategyConfig{
		ID: "hl-lot-tier", Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma", "ETH", "1h", "--mode=paper"}, StopLossATRMult: &slMult,
		Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1,
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
			"sl_after": "breakeven",
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		}},
	}
}

func lotUnifiedTierStrategy() StrategyConfig {
	sc := unifiedSLStrategy("tiered_tp_atr_regime", "perps", false, "breakeven")
	sc.Args = []string{"sma", "ETH", "1h", "--mode=paper"}
	return sc
}

func lotTierPosition(qty float64) *Position {
	return &Position{Symbol: "ETH", Quantity: qty, InitialQuantity: 0.4997, AvgCost: 3000, EntryATR: 30, Side: "long", StopLossTriggerPx: 2970, Regime: "ranging", RegimeAppliedLabel: "ranging"}
}

func TestPaperHLFlooredTierClearsItsThreshold(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"ETH": 4})
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	const mark = 3051.0
	dispatch := func(t *testing.T, sc StrategyConfig, s *StrategyState, fraction, wantClosed float64) {
		t.Helper()
		result := hlLotTestResult("ETH", -1, mark, fraction, 0)
		result.CloseStrategy = sc.CloseStrategy.Name
		if n := paperDispatch(t, sc, s, result, mark); n != 1 {
			t.Fatalf("partial close trades = %d, want 1", n)
		}
		if got := lastTrade(t, s).Quantity; got != wantClosed {
			t.Fatalf("booked close = %v, want %v", got, wantClosed)
		}
	}

	t.Run("tiered_tp_atr floored tier moves the stop to breakeven", func(t *testing.T) {
		sc := lotTierStrategy()
		s := paperStopTestState(sc, lotTierPosition(0.4997))
		dispatch(t, sc, s, 0.5, 0.2498)
		pos := s.Positions["ETH"]
		ratio := 1 - pos.Quantity/pos.InitialQuantity
		if _, exact := findHighestClearedTierByClosedRatio(paperSLAfterTierThresholds(sc, ""), ratio, 0); exact {
			t.Fatalf("closed ratio %.6f already meets 0.5 exactly; the fixture does not reproduce the floored tier", ratio)
		}
		var mu sync.RWMutex
		if !runPaperPostTPStopLossAdjustment(sc, s, "ETH", mark, &Config{}, &mu, nil, silentStrategyLogger(sc.ID)) {
			t.Fatal("floored tier 1 did not move the stop")
		}
		if pos.StopLossTriggerPx != 3000 || pos.SLAdjustedTiersProcessed != 1 {
			t.Fatalf("stop %v processed %d, want breakeven 3000 after tier 1", pos.StopLossTriggerPx, pos.SLAdjustedTiersProcessed)
		}
	})

	t.Run("tiered_tp_atr_regime floored tier records tier 0", func(t *testing.T) {
		sc := lotUnifiedTierStrategy()
		s := paperStopTestState(sc, lotTierPosition(0.4997))
		dispatch(t, sc, s, 0.5, 0.2498)
		pos := s.Positions["ETH"]
		if len(pos.TPConsumptions) != 1 || pos.TPConsumptions[0].Label != "ranging" || pos.TPConsumptions[0].Tier != 0 || pos.TPConsumptions[0].Stage != tpConsumptionBooked {
			t.Fatalf("consumption = %+v, want ranging tier 0 booked", pos.TPConsumptions)
		}
	})

	t.Run("a close more than one lot short does not clear the tier", func(t *testing.T) {
		fraction := 0.24965 / 0.4997
		sc := lotTierStrategy()
		s := paperStopTestState(sc, lotTierPosition(0.4997))
		dispatch(t, sc, s, fraction, 0.2496)
		var mu sync.RWMutex
		if runPaperPostTPStopLossAdjustment(sc, s, "ETH", mark, &Config{}, &mu, nil, silentStrategyLogger(sc.ID)) {
			t.Fatal("0.2496 closed of 0.4997 moved the stop")
		}
		if pos := s.Positions["ETH"]; pos.StopLossTriggerPx != 2970 || pos.SLAdjustedTiersProcessed != 0 {
			t.Fatalf("stop %v processed %d, want the entry stop", pos.StopLossTriggerPx, pos.SLAdjustedTiersProcessed)
		}

		usc := lotUnifiedTierStrategy()
		us := paperStopTestState(usc, lotTierPosition(0.4997))
		dispatch(t, usc, us, fraction, 0.2496)
		if got := us.Positions["ETH"].TPConsumptions; len(got) != 0 {
			t.Fatalf("0.2496 closed of 0.4997 recorded %+v", got)
		}
	})

	t.Run("unknown lot keeps the exact ratio test", func(t *testing.T) {
		if paperTierClearedByLot(0.5, 0.2499, 0.4997, hlLotLookup{}) {
			t.Fatal("unknown lot cleared the tier")
		}
		if !paperTierClearedByLot(0.5, 0.2499, 0.4997, hlLotLookup{Known: true, SzDecimals: 4}) {
			t.Fatal("known lot did not clear the floored tier")
		}
		if paperTierClearedByLot(0.5, 0.4997, 0.4997, hlLotLookup{Known: true, SzDecimals: 0}) {
			t.Fatal("nothing closed cleared the tier")
		}
	})
}

func TestPaperHLHeldPartialCloseRunsQuietCycleMaintenance(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"ETH": 4})
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	leftover := 0.00005 / 0.2499

	t.Run("sub-lot leftover after a floored tier still moves the stop", func(t *testing.T) {
		sc := lotTierStrategy()
		s := paperStopTestState(sc, lotTierPosition(0.2499))
		result := hlLotTestResult("ETH", -1, 3051, leftover, 0)
		requireHeld(t, "leftover close", sc, s, func() int { return paperDispatch(t, sc, s, result, 3051) })
		if result.PaperPartialCloseHold != hlPaperHoldBelowLot {
			t.Fatalf("hold = %q, want %q", result.PaperPartialCloseHold, hlPaperHoldBelowLot)
		}
		if !paperHLHeldPartialCloseNeedsQuietMaintenance(sc, result, 0, 0.2499) {
			t.Fatal("held partial close did not ask for the quiet-cycle upkeep")
		}
		var mu sync.RWMutex
		step := beginHyperliquidStepTradeAlerts(sc, s, &mu)
		runPaperHLQuietCycleMaintenance(sc, s, nil, "ETH", 3051, &Config{}, &mu, nil, silentStrategyLogger(sc.ID), step)
		if pos := s.Positions["ETH"]; pos == nil || pos.StopLossTriggerPx != 3000 || pos.Quantity != 0.2499 {
			t.Fatalf("after upkeep = %+v, want 0.2499 left with the stop at breakeven", pos)
		}
		for _, c := range []struct {
			name   string
			sc     StrategyConfig
			trades int
			qty    float64
			hold   string
		}{
			{"live", StrategyConfig{Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}}, 0, 1, hlPaperHoldBelowLot},
			{"booked", sc, 1, 1, hlPaperHoldBelowLot},
			{"flat", sc, 0, 0, hlPaperHoldBelowLot},
			{"not held", sc, 0, 1, ""},
		} {
			r := &HyperliquidResult{Symbol: "ETH", Signal: -1, PaperPartialCloseHold: c.hold}
			if paperHLHeldPartialCloseNeedsQuietMaintenance(c.sc, r, c.trades, c.qty) {
				t.Fatalf("%s cycle asked for the quiet-cycle upkeep", c.name)
			}
		}
	})

	t.Run("dynamic regime confirms on a held cycle and the new stop closes", func(t *testing.T) {
		dynamic := &StrategyRef{Name: dynamicCloseStrategyName, Params: unifiedBlock()}
		dynamic.Params["regime_confirm_cycles"] = 2
		sc := StrategyConfig{ID: "hl-lot-dyn", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Direction: DirectionLong, CloseStrategy: dynamic}
		pos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 2000, EntryATR: 40, Regime: "trending_down", RegimeAppliedLabel: "trending_up", RegimePendingLabel: "ranging", RegimePendingCount: 1, StopLossTriggerPx: 1940}
		s := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Regime: "ranging", Positions: map[string]*Position{"ETH": pos}}
		result := hlLotTestResult("ETH", -1, 1950, 0.005, 0)
		requireHeld(t, "below-minimum partial close", sc, s, func() int { return paperDispatch(t, sc, s, result, 1950) })
		if result.PaperPartialCloseHold != hlPaperHoldBelowMin || !paperHLHeldPartialCloseNeedsQuietMaintenance(sc, result, 0, 1) {
			t.Fatalf("hold = %q, want %q with the quiet-cycle upkeep", result.PaperPartialCloseHold, hlPaperHoldBelowMin)
		}
		var mu sync.RWMutex
		step := beginHyperliquidStepTradeAlerts(sc, s, &mu)
		runPaperHLQuietCycleMaintenance(sc, s, nil, "ETH", 1950, &Config{}, &mu, nil, silentStrategyLogger(sc.ID), step)
		if pos.RegimeAppliedLabel != "ranging" || pos.StopLossTriggerPx != 1968 {
			t.Fatalf("regime %q stop %v, want ranging with the stop re-armed at 1968", pos.RegimeAppliedLabel, pos.StopLossTriggerPx)
		}
		if s.Positions["ETH"] != nil {
			t.Fatal("the breach at the re-armed stop left the position open")
		}
		if last := lastTrade(t, s); !last.IsClose || math.Abs(last.Price-1950*(1-SlippagePct)) > 1e-9 || last.Quantity != 1 || last.StopLossTriggerPx != 1968 {
			t.Fatalf("stop close = %+v, want the whole position closed at the 1950 mark moved against the sell by SlippagePct, trigger 1968 unslipped", last)
		}
	})

	t.Run("bidirectional held partial with an opposite open stays on its side", func(t *testing.T) {
		sc := lotTierStrategy()
		s := paperStopTestState(sc, lotTierPosition(0.2499))
		result := hlLotTestResult("ETH", -1, 3051, leftover, 0)
		requireHeld(t, "bidirectional leftover close", sc, s, func() int { return paperDispatch(t, sc, s, result, 3051) })
		var mu sync.RWMutex
		step := beginHyperliquidStepTradeAlerts(sc, s, &mu)
		if paperHLHeldPartialCloseNeedsQuietMaintenance(sc, result, 0, 0.2499) {
			runPaperHLQuietCycleMaintenance(sc, s, nil, "ETH", 3051, &Config{}, &mu, nil, silentStrategyLogger(sc.ID), step)
		}
		pos := s.Positions["ETH"]
		if pos == nil || pos.Side != "long" || pos.Quantity != 0.2499 || len(s.Positions) != 1 || len(s.TradeHistory) != 0 {
			t.Fatalf("book = %+v trades %d, want the long kept with no open", s.Positions, len(s.TradeHistory))
		}
		if pos.OpenProfile != "" || pos.ATRMethodAtOpen != "" || pos.DirectionCertifiedAtOpen {
			t.Fatalf("held cycle stamped the position: %+v", pos)
		}
	})
}
