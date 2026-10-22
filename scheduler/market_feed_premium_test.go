package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const pprTestBarMs = int64(60_000)

func pprTestStrategy(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   hyperliquidCheckScript,
		Args:     []string{"donchian_breakout", coin, "1m", "--mode=paper"},
		OpenStrategy: StrategyRef{
			Name:   perpetualPremiumStrategyName,
			Params: map[string]interface{}{"lookback_samples": float64(10)},
		},
	}
}

func pprTestMid(coin string, k int64) float64 {
	if coin == "ETH" {
		switch k {
		case 38:
			return 1998.4
		case 39:
			return 1999.0
		}
		return 2000 + 0.2*float64(k%3)
	}
	switch k {
	case 38:
		return 100.08
	case 39:
		return 100.05
	}
	return 100 + 0.01*float64(k%3)
}

func pprTestHalfSpread(coin string) float64 {
	if coin == "ETH" {
		return 0.05
	}
	return 0.005
}

func pprTestOracle(coin string) string {
	if coin == "ETH" {
		return "2000"
	}
	return "100"
}

func pprDec(v float64) string {
	s, err := canonicalFeedDecimal(fmt.Sprintf("%.4f", v))
	if err != nil {
		panic(err)
	}
	return s
}

func pprLevel(px string, sz string, n int) any {
	return map[string]any{"px": px, "sz": sz, "n": n}
}

func TestPremiumObservationPipeline(t *testing.T) {
	hour := int64(3_600_000)
	B := (time.Now().UTC().UnixMilli()/hour)*hour - 3*hour
	T := func(k int64) int64 { return B + k*60_000 }
	var clockMs atomic.Int64
	clockMs.Store(B - 5*60_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }

	var infoCalls atomic.Int64
	origRate, origHistory, origMids := fetchHyperliquidFundingRateFn, fetchHyperliquidFundingHistoryFn, fetchHyperliquidMidsCtxFn
	fetchHyperliquidFundingRateFn = func(context.Context, string) (float64, error) {
		infoCalls.Add(1)
		return 0, fmt.Errorf("no private fetch is allowed")
	}
	fetchHyperliquidFundingHistoryFn = func(context.Context, string, int64) ([]feedFundingRecord, error) {
		infoCalls.Add(1)
		return nil, fmt.Errorf("no private fetch is allowed")
	}
	fetchHyperliquidMidsCtxFn = func(context.Context, []string) (map[string]float64, error) {
		infoCalls.Add(1)
		return nil, fmt.Errorf("no private fetch is allowed")
	}
	defer func() {
		fetchHyperliquidFundingRateFn, fetchHyperliquidFundingHistoryFn, fetchHyperliquidMidsCtxFn = origRate, origHistory, origMids
	}()

	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		last := clockMs.Load()/pprTestBarMs*pprTestBarMs - pprTestBarMs
		out := make([]hlCandleRaw, 0, 400)
		for i := int64(399); i >= 0; i-- {
			open := last - i*pprTestBarMs
			x := float64(open / pprTestBarMs)
			base := 100.0
			if coin == "ETH" {
				base = 2000
			}
			c := base * (1 + 0.001*math.Sin(x*0.7))
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + pprTestBarMs - 1, HasClose: true,
				Open: c * 0.9999, High: c * 1.0005, Low: c * 0.9995, Close: c, Volume: 10})
		}
		return out, nil
	})

	btc := pprTestStrategy("ppr-btc", "BTC")
	eth := pprTestStrategy("ppr-eth", "ETH")
	ethOI := oiTestStrategy("oi-eth", "ETH")
	cfg := &Config{Strategies: []StrategyConfig{btc, eth, ethOI}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	btcKey, ethKey, ethOIKey := premiumKeyFor("BTC"), premiumKeyFor("ETH"), openInterestKeyFor("ETH")
	wantWindow := 11*feedPremiumSampleMs + pprTestBarMs
	if req.Observations[btcKey] != wantWindow || req.Observations[ethKey] != wantWindow || req.Observations[ethOIKey] == 0 {
		t.Fatalf("observation requirements %+v, want premium windows %dms and an ETH open-interest window", req.Observations, wantWindow)
	}
	if e := req.Strategies["ppr-btc"]; !e.Premium || e.OpenInterest || e.TradeFlow || e.PremiumWindow != wantWindow || e.FundingScalar || e.FundingRecords {
		t.Fatalf("a premium-only requirement must need premium records with both funding flags false: %+v", e)
	}
	if f, ok := req.Funding["BTC"]; ok && (f.Scalar || f.Records) {
		t.Fatalf("a premium strategy must not request funding: %+v", f)
	}
	bad := pprTestStrategy("ppr-bad", "BTC")
	bad.OpenStrategy.Params = map[string]interface{}{"lookback_samples": float64(5000)}
	if _, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{bad}}); err == nil || !strings.Contains(err.Error(), "lookback_samples") {
		t.Fatalf("an oversized premium lookback must fail preflight, got %v", err)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{btc}}); err == nil || !strings.Contains(err.Error(), "synchronized quote and oracle records") {
		t.Fatalf("market_feed=rest with a premium strategy must be refused as an unsupported mode: %v", err)
	}
	served := dropUncollectableObservations(feedSourceREST, []feedConsumer{{Path: "ppr", Loaded: true, Req: req}})
	if !served[0].Loaded || len(served[0].Req.Observations) != 0 || served[0].Req.Strategies["ppr-btc"].Premium || len(served[0].Req.Order) != len(req.Order) ||
		!strings.Contains(served[0].Notice, "quote/oracle") {
		t.Fatalf("a REST feed source must drop only the observation needs and say why: %+v", served[0])
	}
	union := unionFeedRequirements([]feedConsumer{{Path: "ppr", Loaded: true, Req: req}})
	estimate := estimateFeedSealBytes(union)

	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()

	owner := newMarketFeedOwner(clock, t.Logf)
	owner.corrOffsets = nil
	select {
	case <-owner.ApplyGeneration(context.Background(), req):
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not publish")
	}
	tmp := t.TempDir()
	recDir := filepath.Join(tmp, "recording")
	rec, err := newObservationRecorder(recDir, observationRecorderMeta{
		RunID: "premium-harness", Coins: []string{"BTC", "ETH"}, Kinds: []string{feedObservationPerpPremium, feedObservationOpenInterest},
		Segment: 20 * time.Minute, WSURL: fs.url(), StartedAt: clock(),
	}, clock, t.Logf)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	owner.attachRecorder(rec)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "premium subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `"coin":"BTC","type":"bbo"`) && strings.Contains(subs, `"coin":"ETH","type":"bbo"`) &&
			strings.Contains(subs, `"coin":"BTC","type":"activeAssetCtx"`) && strings.Count(subs, `"coin":"ETH","type":"activeAssetCtx"`) == 1
	})

	pstate := func(key feedObservationKey) feedPremiumState {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		st := owner.premiums[key]
		cp := *st
		cp.Samples = nil
		for _, smp := range st.Samples {
			c := *smp
			cp.Samples = append(cp.Samples, &c)
		}
		cp.Gaps = append([]feedObservationGap(nil), st.Gaps...)
		return cp
	}
	oiStats := func() feedObservationStats {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.observations[ethOIKey].Stats
	}
	sendBBO := func(coin string, recvMs, eventMs int64, levels ...any) {
		t.Helper()
		key := premiumKeyFor(coin)
		before := pstate(key).Stats.QuoteFrames
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "bbo", "data": map[string]any{"coin": coin, "time": eventMs, "bbo": levels}})
		waitFor(t, fmt.Sprintf("%s bbo at %d", coin, recvMs), func() bool { return pstate(key).Stats.QuoteFrames > before })
	}
	quoteLevels := func(coin string, k int64) []any {
		mid := pprTestMid(coin, k)
		h := pprTestHalfSpread(coin)
		return []any{pprLevel(pprDec(mid-h), "1.5", 3), pprLevel(pprDec(mid+h), "2.25", 4)}
	}
	sendCtx := func(coin string, recvMs int64, ctxFields map[string]any) {
		t.Helper()
		key := premiumKeyFor(coin)
		before := pstate(key).Stats.CtxFrames
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": coin, "ctx": ctxFields}})
		waitFor(t, fmt.Sprintf("%s activeAssetCtx at %d", coin, recvMs), func() bool { return pstate(key).Stats.CtxFrames > before })
	}
	normalCtx := func(coin string, k int64) map[string]any {
		mid := pprDec(pprTestMid(coin, k))
		return map[string]any{"oraclePx": pprTestOracle(coin), "markPx": mid, "midPx": mid, "openInterest": fmt.Sprintf("%d.5", 5000+k),
			"funding": "0.0000125", "premium": "0.0001", "impactPxs": []string{mid, mid}}
	}
	minute := func(coin string, k int64) {
		t.Helper()
		sendCtx(coin, T(k)-2_000, normalCtx(coin, k))
		sendBBO(coin, T(k)-1_000, T(k)-1_200, quoteLevels(coin, k)...)
	}

	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-ppr", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{60}, time.UnixMilli(B-hour))
	sealer.startServing(time.UnixMilli(B - hour))
	sockDir, err := os.MkdirTemp("", "ppr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "f.sock")
	srv, err := listenFeedSocket(sock, sealer, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	go srv.serve()
	defer srv.close()

	key1, keyE, key2 := T(26)/1000, T(39)/1000, T(43)/1000
	seal := func(key int64, at int64) []byte {
		t.Helper()
		clockMs.Store(at)
		sealer.sealOne(ctx, key)
		blob := append([]byte(nil), sealer.sealBytes(key)...)
		if len(blob) == 0 {
			t.Fatalf("no seal for key %d: %+v", key, sealer.lookup(key))
		}
		return blob
	}
	seen := map[feedObservationKey]map[int64]*feedPremiumSample{btcKey: {}, ethKey: {}}
	remember := func() {
		for key, hist := range seen {
			for _, smp := range pstate(key).Samples {
				hist[smp.T] = smp
			}
		}
	}
	var sealed1, sealedE []byte
	for k := int64(1); k <= 40; k++ {
		for _, coin := range []string{"BTC", "ETH"} {
			switch {
			case coin == "BTC" && k == 22:
				sendCtx(coin, T(k)-2_000, normalCtx(coin, k))
			case coin == "ETH" && k == 13:
				fields := normalCtx(coin, k)
				fields["oraclePx"] = "abc"
				oi := oiStats().Accepted
				sendCtx(coin, T(k)-2_000, fields)
				if oiStats().Accepted != oi+1 {
					t.Fatalf("a malformed oracle field must not block a valid open-interest field: %+v", oiStats())
				}
				sendBBO(coin, T(k)-1_000, T(k)-1_200, quoteLevels(coin, k)...)
			case coin == "ETH" && k == 14:
				fields := normalCtx(coin, k)
				fields["openInterest"] = "bad"
				oi := oiStats().Rejected
				sendCtx(coin, T(k)-2_000, fields)
				if oiStats().Rejected != oi+1 {
					t.Fatalf("a malformed open-interest field must be rejected by its owner: %+v", oiStats())
				}
				sendBBO(coin, T(k)-1_000, T(k)-1_200, quoteLevels(coin, k)...)
			default:
				minute(coin, k)
			}
			if coin != "BTC" {
				continue
			}
			switch k {
			case 3:
				sendBBO(coin, T(k)-900, T(k)-1_200, quoteLevels(coin, k)...)
			case 5:
				sendBBO(coin, T(k)+500, T(k)-1_200, pprLevel("99.9", "1", 1), pprLevel(pprDec(pprTestMid(coin, k)+0.005), "2.25", 4))
			case 6:
				sendBBO(coin, T(k)-800, T(5)-1_200, quoteLevels(coin, 5)...)
			case 8:
				sendBBO(coin, T(k)-800, T(k)-5_000, quoteLevels(coin, k)...)
			case 10:
				sendBBO(coin, T(k)-800, T(k)+20_000, quoteLevels(coin, k)...)
			case 12:
				sendBBO(coin, T(k)-800, T(k)-900, pprLevel("abc", "1", 1), pprLevel("100.1", "1", 1))
			case 15:
				sendBBO(coin, T(k)-800, T(k)-900, pprLevel("100", "1", 1), nil)
			case 16:
				sendBBO(coin, T(k)-800, T(k)-900, pprLevel("100.02", "1", 1), pprLevel("100.01", "1", 1))
			case 18:
				sendCtx(coin, T(k)-3_000, normalCtx(coin, k))
			case 24:
				sendBBO(coin, T(k)-700, T(k)-1_200, pprLevel("99.9", "1", 1), pprLevel("100.1", "1", 1))
				sendBBO(coin, T(k)-600, T(k)-1_200, quoteLevels(coin, k)...)
			}
		}
		if k == 20 {
			before := pstate(ethKey).Stats.Rejected
			clockMs.Store(T(k) - 500)
			fs.broadcastRaw(t, []byte("not a json envelope"))
			waitFor(t, "unparsable frame", func() bool { return pstate(ethKey).Stats.Rejected > before })
		}
		if k == 26 {
			sealed1 = seal(key1, T(26)+2_000)
		}
		if k == 39 {
			sealedE = seal(keyE, T(39)+2_000)
		}
		remember()
	}
	remember()

	clockMs.Store(T(40) + 3_000)
	sessions := fs.sessionCount()
	fs.dropConnections()
	waitFor(t, "disconnect", func() bool { return !owner.Connected() })
	waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
	waitFor(t, "resubscribe", func() bool {
		return strings.Count(strings.Join(fs.subscriptions(), " "), `"coin":"BTC","type":"bbo"`) > 1
	})
	if st := pstate(btcKey); st.quote != nil || st.ctx != nil || len(st.Gaps) == 0 || st.Gaps[len(st.Gaps)-1].Reason != feedPremiumReasonDisconnected {
		t.Fatalf("a disconnect must clear both components and open a dated gap: %+v", st.Gaps)
	}
	for k := int64(41); k <= 43; k++ {
		minute("BTC", k)
		minute("ETH", k)
	}
	sealed2 := seal(key2, T(43)+2_000)
	remember()

	st := pstate(btcKey)
	byT := seen[btcKey]
	expect := func(key feedObservationKey, k int64, status, reason string) *feedPremiumSample {
		t.Helper()
		smp := seen[key][T(k)]
		if smp == nil {
			t.Fatalf("%s sample %d is missing", key.PayloadID(), k)
		}
		if smp.Status != status || smp.Reason != reason {
			t.Fatalf("%s sample %d: %s %q, want %s %q (%+v)", key.PayloadID(), k, smp.Status, smp.Reason, status, reason, smp)
		}
		return smp
	}
	first := expect(btcKey, 1, feedPremiumStatusSampled, "")
	if first.Quote.EventMs != T(1)-1_200 || first.Quote.RecvMs != T(1)-1_000 || first.Context.RecvMs != T(1)-2_000 || first.Context.MidPx == "" {
		t.Fatalf("a sample must hold the latest components with their own receipt and venue times: %+v %+v", first.Quote, first.Context)
	}
	expect(btcKey, 3, feedPremiumStatusSampled, "")
	if st.Stats.QuoteDuplicates != 1 {
		t.Fatalf("an identical bbo resend must be idempotent: %+v", st.Stats)
	}
	s5 := expect(btcKey, 5, feedPremiumStatusSampled, "")
	if s5.InvalidMs != T(5)+500 || s5.InvalidWhy != feedPremiumReasonConflict {
		t.Fatalf("a conflicting duplicate must revoke the earlier sample that used that venue time, dated: %+v", s5)
	}
	expect(btcKey, 6, feedPremiumStatusInvalid, feedPremiumReasonConflict)
	expect(btcKey, 7, feedPremiumStatusSampled, "")
	expect(btcKey, 8, feedPremiumStatusInvalid, feedPremiumReasonOutOfOrder)
	expect(btcKey, 10, feedPremiumStatusInvalid, feedPremiumReasonFuture)
	expect(btcKey, 12, feedPremiumStatusInvalid, feedPremiumReasonMalformed)
	expect(ethKey, 13, feedPremiumStatusInvalid, feedPremiumReasonMalformed)
	expect(ethKey, 14, feedPremiumStatusSampled, "")
	expect(btcKey, 15, feedPremiumStatusMissing, feedPremiumReasonOneSided)
	expect(btcKey, 16, feedPremiumStatusInvalid, feedPremiumReasonCrossed)
	expect(btcKey, 18, feedPremiumStatusInvalid, feedPremiumReasonClock)
	expect(btcKey, 20, feedPremiumStatusInvalid, feedPremiumReasonMalformed)
	expect(ethKey, 20, feedPremiumStatusInvalid, feedPremiumReasonMalformed)
	expect(btcKey, 24, feedPremiumStatusInvalid, feedPremiumReasonConflict)
	expect(btcKey, 25, feedPremiumStatusSampled, "")
	stale := expect(btcKey, 22, feedPremiumStatusSampled, "")
	if T(22)-stale.Quote.RecvMs < 60_000 {
		t.Fatalf("a quote-less minute must keep the older quote with its own receipt time for the evaluator to age: %+v", stale.Quote)
	}
	expect(btcKey, 40, feedPremiumStatusSampled, "")
	after := expect(btcKey, 41, feedPremiumStatusSampled, "")
	if after.Quote.Session == byT[T(40)].Quote.Session {
		t.Fatalf("records after a reconnect must carry the new session: %+v", after.Quote)
	}
	if r := premiumReadiness(&st, T(43)+2_000); r.Ready || r.Status != feedObservationStatusWarming || r.SampledRun != 3 {
		t.Fatalf("a reconnect must rebuild a complete single-session window before readiness: %+v", r)
	}
	if st.Stats.QuoteConflicts != 3 || st.Stats.QuoteOutOfOrder != 2 || st.Stats.QuoteOneSided != 1 || st.Stats.QuoteDuplicates != 1 {
		t.Fatalf("outcome counters %+v", st.Stats)
	}
	if infoCalls.Load() != 0 {
		t.Fatalf("the premium pipeline made %d /info requests; it must use only the feed-owned subscriptions", infoCalls.Load())
	}

	doc1, err := decodeFeedSeal(sealed1, key1)
	if err != nil {
		t.Fatalf("decode seal 1: %v", err)
	}
	if doc1.V != feedSealVersionPremium || len(doc1.Premiums) != 2 || len(doc1.Observations) != 1 {
		t.Fatalf("a combined premium and open-interest seal must be v%d: v%d with %d premium and %d open-interest entries",
			feedSealVersionPremium, doc1.V, len(doc1.Premiums), len(doc1.Observations))
	}
	for _, smp := range doc1.Premiums[0].Samples {
		if smp.T > T(26) {
			t.Fatalf("seal 1 holds a sample after its cutoff: %+v", smp)
		}
	}
	docE, err := decodeFeedSeal(sealedE, keyE)
	if err != nil || len(docE.Premiums) != 2 || docE.Premiums[0].CutoffMs != T(39) || docE.Premiums[0].Samples[len(docE.Premiums[0].Samples)-1].T != T(39) {
		t.Fatalf("the decision seal must end at its deadline sample: %v", err)
	}
	if !bytes.Equal(sealed1, seal(key1, T(43)+3_000)) {
		t.Fatal("later records or a second seal attempt changed immutable seal bytes")
	}
	for name, mutate := range map[string]func(*feedSealDoc){
		"version":  func(d *feedSealDoc) { d.V = feedSealVersionMacro },
		"strip":    func(d *feedSealDoc) { d.Premiums = nil },
		"contract": func(d *feedSealDoc) { d.Premiums[0].Contract = "other" },
		"kind":     func(d *feedSealDoc) { d.Premiums[0].Kind = feedObservationOpenInterest },
		"source":   func(d *feedSealDoc) { d.Premiums[0].Source = feedObservationSourceHLWS },
		"crossed":  func(d *feedSealDoc) { d.Premiums[0].Samples[0].Quote.Bid = d.Premiums[0].Samples[0].Quote.Ask },
		"future":   func(d *feedSealDoc) { d.Premiums[0].Samples[0].Quote.RecvMs = d.Premiums[0].Samples[0].T + 1 },
		"order":    func(d *feedSealDoc) { d.Premiums[0].Samples[1].T = d.Premiums[0].Samples[0].T },
		"sampled":  func(d *feedSealDoc) { d.Premiums[0].Samples[0].Context = nil },
		"decimal":  func(d *feedSealDoc) { d.Premiums[0].Samples[0].Context.Oracle += ".0" },
		"final":    func(d *feedSealDoc) { d.Premiums[0].Samples[0].FinalizedMs = d.Premiums[0].Samples[0].T },
	} {
		cp, _ := decodeFeedSeal(sealed1, key1)
		mutate(cp)
		if blob, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(blob, key1); err == nil {
				t.Fatalf("a tampered or relabeled premium seal (%s) decoded", name)
			}
		}
	}
	if len(sealed2) > estimate || len(sealed2) > feedSealMaxBytes {
		t.Fatalf("seal 2 is %d bytes; estimate %d, cap %d", len(sealed2), estimate, feedSealMaxBytes)
	}

	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, 6*time.Hour
	descs := client.Describe(ctx)
	if descs[0].Err != nil || descs[0].Header.SealVersion != feedSealVersionPremium {
		t.Fatalf("describe: %+v", descs[0])
	}
	premiumDescribed := 0
	for _, ob := range descs[0].Describe.Observations {
		if ob.Kind == feedObservationPerpPremium && ob.Source == feedPremiumSourceHLWS && ob.WindowMs == wantWindow {
			premiumDescribed++
		}
	}
	if premiumDescribed != 2 {
		t.Fatalf("describe must advertise both premium keys with their source and window: %+v", descs[0].Describe.Observations)
	}
	if _, problems := sharedFeedCompatibilityLines("test", descs, req, []int{60}); len(problems) != 0 {
		t.Fatalf("a feed serving the premium windows reported coverage problems: %v", problems)
	}
	wider := req
	wider.Observations = map[feedObservationKey]int64{btcKey: wantWindow * 2}
	if _, problems := sharedFeedCompatibilityLines("test", descs, wider, []int{60}); len(problems) == 0 || !strings.Contains(problems[0], "BTC|perp_premium window") {
		t.Fatalf("a consumer needing a longer premium window must see a coverage gap, got %v", problems)
	}
	reqs := cycleRequirementsForDue([]StrategyConfig{btc, eth, ethOI}, req)
	fetchPayload := func(key int64, sc StrategyConfig) *marketPayload {
		t.Helper()
		snap, rep := client.Fetch(ctx, key, reqs)
		if rep.Status != feedFetchSealed || len(rep.Gaps) != 0 {
			t.Fatalf("fetch key %d: %+v", key, rep)
		}
		feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, SharedKey: key}
		if hold := feed.holdFor(sc); hold.Held {
			t.Fatalf("%s with a ready candle frame was held: %+v", sc.ID, hold)
		}
		return oiTestPayload(t, feed, sc)
	}
	payloads := map[string]*marketPayload{
		"payload_btc_1.json": fetchPayload(key1, btc), "payload_btc_e.json": fetchPayload(keyE, btc),
		"payload_eth_e.json": fetchPayload(keyE, eth), "payload_btc_2.json": fetchPayload(key2, btc),
	}
	pe := payloads["payload_btc_e.json"]
	pp := pe.Premiums["BTC|perp_premium"]
	if pe.Version != marketSnapshotVersion || !pp.Available || pp.CutoffMs != T(39) || pp.WindowMs != wantWindow || pp.Contract != feedPremiumContract || len(pp.Samples) == 0 {
		t.Fatalf("BTC premium payload: v%d %+v", pe.Version, pp)
	}
	for _, smp := range pp.Samples {
		if smp.Context != nil && smp.Context.OracleEventMs != nil {
			t.Fatalf("an absent oracle source time must stay null, never a receipt time: %+v", smp.Context)
		}
	}
	if _, ok := pe.Frames["BTC|1m"]; !ok {
		t.Fatal("the BTC candle frame must stay in the payload")
	}
	if len(pe.Observations) != 0 || len(pe.TradeFlows) != 0 {
		t.Fatal("a premium strategy payload must not carry another strategy's observations")
	}
	oiPayload := fetchPayload(key2, ethOI)
	if oiPayload.Version != marketSnapshotVersion || oiPayload.Premiums != nil || !oiPayload.Observations["ETH|open_interest"].Available {
		t.Fatalf("an open-interest payload must keep version %d, carry no premium records and stay available: v%d", marketSnapshotVersion, oiPayload.Version)
	}
	missing := (&marketSnapshot{EvaluationID: "x", premiums: map[feedObservationKey]*marketSnapshotPremium{}}).premiumPayload(premiumKeyFor("SOL"), wantWindow, pprTestBarMs)
	if missing.Available || !strings.Contains(missing.Reason, "not in the sealed snapshot") {
		t.Fatalf("an unsealed premium key must be unavailable with a reason: %+v", missing)
	}
	short := (&marketSnapshot{EvaluationID: "x", premiums: map[feedObservationKey]*marketSnapshotPremium{btcKey: {Key: btcKey, WindowMs: 60_000, CutoffMs: T(39)}}}).premiumPayload(btcKey, wantWindow, pprTestBarMs)
	if short.Available || !strings.Contains(short.Reason, "below the") {
		t.Fatalf("a short sealed window must be unavailable with a reason: %+v", short)
	}

	for _, tc := range []struct {
		sealVersion int
		compatible  bool
	}{{feedSealVersionBase, true}, {feedSealVersionMacro, true}, {feedSealVersionProfile, true}, {feedSealVersionPremium, true}, {feedSealVersionPremium + 1, false}} {
		peer := oiTestOldPeer(t, sockDir, tc.sealVersion)
		_, _, err := client.roundTrip(ctx, peer, feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe})
		if tc.compatible != (err == nil) {
			t.Fatalf("a seal-v%d peer: compatible=%v, err %v", tc.sealVersion, tc.compatible, err)
		}
	}

	rejected := map[string]uint64{"BTC": pstate(btcKey).Stats.Rejected, "ETH": pstate(ethKey).Stats.Rejected}
	restarted := newMarketFeedOwner(clock, t.Logf)
	restarted.applyObservationNeedsLocked(map[feedObservationKey]int64{btcKey: wantWindow})
	for k := int64(44); k <= 46; k++ {
		clockMs.Store(T(k) - 2_000)
		ctxBlob, _ := json.Marshal(map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": "BTC", "ctx": normalCtx("BTC", k)}})
		restarted.handleSocketMessage(ctxBlob)
		clockMs.Store(T(k) - 1_000)
		bboBlob, _ := json.Marshal(map[string]any{"channel": "bbo", "data": map[string]any{"coin": "BTC", "time": T(k) - 1_200, "bbo": quoteLevels("BTC", k)}})
		restarted.handleSocketMessage(bboBlob)
	}
	clockMs.Store(T(46) + 500)
	restarted.AdvancePremiums(clock())
	if r := restarted.PremiumReadiness(); len(r) != 1 || r[0].Ready || r[0].Status != feedObservationStatusWarming || r[0].Samples != 3 {
		t.Fatalf("a restarted owner must rebuild its window from new samples and stay unready: %+v", r)
	}
	cancel()
	waitFor(t, "disconnect", func() bool { return !owner.Connected() })
	owner.attachRecorder(nil)
	stopRec()
	if err := rec.wait(); err != nil {
		t.Fatalf("recorder: %v", err)
	}
	if !rec.manifest.Closed || rec.manifest.DroppedTotal != 0 {
		t.Fatalf("recorder manifest: closed=%t dropped=%d", rec.manifest.Closed, rec.manifest.DroppedTotal)
	}
	reload := req
	reload.Observations = map[feedObservationKey]int64{ethOIKey: req.Observations[ethOIKey]}
	owner.feedMu.Lock()
	owner.dropUnneededObservationsLocked(reload.Observations)
	_, kept := owner.premiums[btcKey]
	owner.applyObservationNeedsLocked(map[feedObservationKey]int64{btcKey: wantWindow})
	fresh := len(owner.premiums[btcKey].Samples)
	owner.feedMu.Unlock()
	if kept || fresh != 0 {
		t.Fatalf("a reload that removes the premium need must drop its history; a later need starts empty (kept=%v, samples=%d)", kept, fresh)
	}

	if out := os.Getenv("GO_TRADER_PREMIUM_HARNESS_OUT"); out != "" {
		pprWriteHarness(t, out, recDir, B, payloads, rejected)
	}
}

func TestPremiumCapacityAtMaximumWindow(t *testing.T) {
	window := feedPremiumMaxWindowMs
	cutoff := int64(1_800_000_000_000)
	st := newFeedPremiumState(premiumKeyFor("BTC"), window)
	for ts := cutoff - window; ts <= cutoff; ts += feedPremiumSampleMs {
		st.Samples = append(st.Samples, &feedPremiumSample{
			T: ts, Status: feedPremiumStatusSampled, FinalizedMs: ts + 1,
			Quote: &feedPremiumQuote{BidPx: "123456.123456789", BidSz: "123456.12345678", BidN: 999999, AskPx: "123456.123456799", AskSz: "123456.12345678",
				AskN: 999999, EventMs: ts - 200, RecvMs: ts - 1, Session: 99999, Seq: 999_999_999},
			Context:   &feedPremiumContext{OraclePx: "123456.123456789", MarkPx: "123456.123456789", MidPx: "123456.123456789", RecvMs: ts - 1, Session: 99999, Seq: 999_999_999},
			InvalidMs: ts + 2, InvalidWhy: feedPremiumReasonConflict,
		})
	}
	snap := &marketSnapshot{EvaluationID: feedSealEvaluationID(cutoff / 1000), SealedAt: time.UnixMilli(cutoff), Deadline: time.UnixMilli(cutoff),
		keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{},
		observations: map[feedObservationKey]*marketSnapshotObservation{}, premiums: map[feedObservationKey]*marketSnapshotPremium{}}
	for _, coin := range []string{"BTC", "ETH", "SOL", "XRP"} {
		cp := *st
		cp.Key = premiumKeyFor(coin)
		snap.premiums[cp.Key] = freezePremium(&cp, window, cutoff, cutoff)
	}
	doc, err := feedSealDocFromSnapshot(snap, cutoff/1000, feedSourceWebsocket, "cap")
	if err != nil {
		t.Fatal(err)
	}
	blob, _, err := encodeFeedSeal(doc)
	if err != nil {
		t.Fatalf("a maximum-window four-coin premium seal must encode under the cap: %v", err)
	}
	union := feedRequirements{Observations: map[feedObservationKey]int64{}}
	for k := range snap.premiums {
		union.Observations[k] = window
	}
	if est := estimateFeedSealBytes(union); len(blob) > est {
		t.Fatalf("seal %d bytes exceeds the capacity estimate %d", len(blob), est)
	}
	if _, err := decodeFeedSeal(blob, cutoff/1000); err != nil {
		t.Fatalf("decode the maximum seal: %v", err)
	}
	payload := &marketPayload{Version: marketSnapshotVersion, SnapshotID: snap.EvaluationID, Frames: map[string]marketFrame{}}
	needs := map[feedObservationKey]int64{}
	for k := range snap.premiums {
		needs[k] = window
	}
	attachObservationPayloads(payload, snap, needs, pprTestBarMs)
	batch, err := marketStdinJSON(payload)
	if err != nil || len(payload.Premiums) != 4 || payload.Version != marketSnapshotVersion {
		t.Fatalf("a four-coin maximum batch payload must fit the %d-byte cap as version %d: %d bytes, %v", marketPayloadMaxBytes, marketSnapshotVersion, len(batch), err)
	}
}

func pprWriteHarness(t *testing.T, out, recDir string, base int64, payloads map[string]*marketPayload, rejected map[string]uint64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(out, "recording"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(recDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		blob, err := os.ReadFile(filepath.Join(recDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "recording", e.Name()), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range payloads {
		blob, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	T := func(k int64) int64 { return base + k*60_000 }
	cases := []map[string]any{
		{"payload": "payload_btc_1.json", "coin": "BTC", "decision_ms": T(26), "signal": 0},
		{"payload": "payload_btc_e.json", "coin": "BTC", "decision_ms": T(39), "signal": -1, "reason": "entry_short"},
		{"payload": "payload_eth_e.json", "coin": "ETH", "decision_ms": T(39), "signal": 1, "reason": "entry_long"},
		{"payload": "payload_btc_2.json", "coin": "BTC", "decision_ms": T(43), "signal": 0, "reason": "session_change"},
	}
	harness := map[string]any{"timeframe": "1m", "params": map[string]any{"lookback_samples": 10}, "base_ms": base,
		"expected_rejected": rejected, "cases": cases}
	blob, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "harness.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func pprRecordV3Run(t *testing.T, runID string, kinds []string) string {
	t.Helper()
	B := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(B - 30_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	coins := []string{"BTC", "ETH"}
	has := func(kind string) bool { return recorderKindsInclude(kinds, kind) }
	recDir := filepath.Join(t.TempDir(), runID)
	rec, err := newObservationRecorder(recDir, observationRecorderMeta{RunID: runID, Coins: coins, Kinds: kinds,
		Segment: 3 * time.Minute, WSURL: fs.url(), StartedAt: clock()}, clock, t.Logf)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.bookTick = l2TestTickPeriod
	if has(feedObservationOpenInterest) {
		owner.trackObservations(coins, observationRecorderWindow.Milliseconds())
	}
	if has(feedObservationPerpPremium) {
		owner.trackPremiums(coins, observationRecorderWindow.Milliseconds())
	}
	owner.attachRecorder(rec)
	if has(feedObservationL2Book) {
		owner.trackBooks(coins, recorderBookNeed())
	}
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "every subscription", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		for _, c := range coins {
			want := []string{}
			if has(feedObservationOpenInterest) || has(feedObservationPerpPremium) {
				want = append(want, `"coin":"`+c+`","type":"activeAssetCtx"`)
			}
			if has(feedObservationPerpPremium) {
				want = append(want, `"coin":"`+c+`","type":"bbo"`)
			}
			if has(feedObservationL2Book) {
				want = append(want, `"coin":"`+c+`","fast":false,"type":"l2Book"`)
			}
			for _, w := range want {
				if !strings.Contains(subs, w) {
					return false
				}
			}
		}
		return true
	})
	counts := func() uint64 {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		var n uint64
		for _, c := range coins {
			if st := owner.observations[openInterestKeyFor(c)]; st != nil {
				n += st.Stats.Received
			}
			if st := owner.premiums[premiumKeyFor(c)]; st != nil {
				n += st.Stats.QuoteFrames + st.Stats.CtxFrames
			}
			if st := owner.books[l2BookKeyFor(c)]; st != nil {
				n += st.Stats.Frames
			}
		}
		return n
	}
	perStep := uint64(0)
	if has(feedObservationOpenInterest) {
		perStep++
	}
	if has(feedObservationPerpPremium) {
		perStep += 2
	}
	if has(feedObservationL2Book) {
		perStep++
		clockMs.Store(B - 20_000)
		for _, c := range coins {
			fs.broadcast(t, l2TestAck(c, nil))
		}
		waitFor(t, "book acknowledgements", func() bool {
			owner.feedMu.Lock()
			defer owner.feedMu.Unlock()
			for _, c := range coins {
				if !owner.books[l2BookKeyFor(c)].Covered {
					return false
				}
			}
			return true
		})
	}
	tag := 0
	for t0 := B - 15_000; t0 <= B+6*60_000; t0 += 5_000 {
		for i, c := range coins {
			recv := t0 + int64(i)*100
			before := counts()
			mid := 100 + 0.01*float64((t0/5_000)%3)
			if c == "ETH" {
				mid = 2000 + 0.2*float64((t0/5_000)%3)
			}
			h := pprTestHalfSpread(c)
			clockMs.Store(recv)
			if has(feedObservationOpenInterest) || has(feedObservationPerpPremium) {
				fs.broadcast(t, map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": c, "ctx": map[string]any{
					"oraclePx": pprTestOracle(c), "markPx": pprDec(mid), "midPx": pprDec(mid), "openInterest": fmt.Sprintf("%d.5", 5000+t0%7),
					"funding": "0.0000125", "premium": "0.0001", "impactPxs": []string{pprDec(mid), pprDec(mid)}}}})
			}
			if has(feedObservationPerpPremium) {
				fs.broadcast(t, map[string]any{"channel": "bbo", "data": map[string]any{"coin": c, "time": recv - 200,
					"bbo": []any{pprLevel(pprDec(mid-h), "1.5", 3), pprLevel(pprDec(mid+h), "2.25", 4)}}})
			}
			if has(feedObservationL2Book) {
				fs.broadcast(t, l2TestBook(c, recv-300, tag))
				tag++
			}
			waitFor(t, fmt.Sprintf("%s frames at %d", c, recv), func() bool { return counts() >= before+perStep })
		}
		owner.AdvancePremiums(clock())
	}
	clockMs.Store(B + 6*60_000 + 30_000)
	owner.AdvancePremiums(clock())
	cancel()
	waitFor(t, "disconnect", func() bool { return !owner.Connected() })
	owner.attachRecorder(nil)
	stopRec()
	if err := rec.wait(); err != nil {
		t.Fatalf("recorder: %v", err)
	}
	if !rec.manifest.Closed || rec.manifest.DroppedTotal != 0 {
		t.Fatalf("recorder manifest: closed=%t dropped=%d", rec.manifest.Closed, rec.manifest.DroppedTotal)
	}
	return recDir
}

func TestV3RecordingSchemaTypesEachKind(t *testing.T) {
	cases := []struct {
		name  string
		kinds []string
	}{
		{"premium", []string{feedObservationPerpPremium}},
		{"book", []string{feedObservationL2Book}},
		{"mixed", []string{feedObservationL2Book, feedObservationOpenInterest, feedObservationPerpPremium}},
	}
	out := os.Getenv("GO_TRADER_V3_KINDS_OUT")
	for _, tc := range cases {
		dir := pprRecordV3Run(t, "v3-"+tc.name, tc.kinds)
		wantBook, wantPremium := recorderKindsInclude(tc.kinds, feedObservationL2Book), recorderKindsInclude(tc.kinds, feedObservationPerpPremium)
		check := func(label string, blob []byte) {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(blob, &doc); err != nil {
				t.Fatalf("%s %s: %v", tc.name, label, err)
			}
			_, book := doc["book_contract"]
			_, premium := doc["premium_contract"]
			var kinds []string
			_ = json.Unmarshal(doc["kinds"], &kinds)
			if book != wantBook || premium != wantPremium || strings.Join(kinds, ",") != strings.Join(tc.kinds, ",") {
				t.Fatalf("%s %s must carry exactly the typed contract blocks its kinds need (book=%t premium=%t kinds=%v): %s",
					tc.name, label, wantBook, wantPremium, tc.kinds, blob)
			}
		}
		runBlob, err := os.ReadFile(filepath.Join(dir, "run.manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var run observationRunManifestV2
		if err := json.Unmarshal(runBlob, &run); err != nil || run.Schema != observationRunSchemaV3 || len(run.Segments) < 2 {
			t.Fatalf("%s run manifest must be schema v3 with several segments: %s (%v)", tc.name, runBlob, err)
		}
		check("run manifest", runBlob)
		for _, seg := range run.Segments {
			if seg.Schema != observationSegmentSchemaV3 {
				t.Fatalf("%s segment %s schema %q", tc.name, seg.File, seg.Schema)
			}
			blob, err := os.ReadFile(filepath.Join(dir, seg.File))
			if err != nil {
				t.Fatal(err)
			}
			first, _, _ := bytes.Cut(blob, []byte("\n"))
			check("segment "+seg.File+" header", first)
		}
		if out != "" {
			dst := filepath.Join(out, tc.name)
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				blob, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dst, e.Name()), blob, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestSealWithBooksAndPremiumsIsV8AndPayloadV4CarriesBoth(t *testing.T) {
	l2RegisterFixture(t)
	B := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(B - 20*60_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	l2StubCandles(t, &clockMs)
	book := l2TestStrategyConfig("l2-btc", "BTC", map[string]interface{}{"window_ms": float64(60_000), "capacity": float64(64), "depth": float64(5)})
	prem := pprTestStrategy("ppr-btc", "BTC")
	req, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{book, prem}})
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	if len(req.Books) != 1 || req.Observations[premiumKeyFor("BTC")] == 0 {
		t.Fatalf("the union must need one book and one premium key: books=%v observations=%v", req.Books, req.Observations)
	}
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.corrOffsets = nil
	owner.bookTick = l2TestTickPeriod
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	select {
	case <-owner.ApplyGeneration(ctx, req):
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not publish")
	}
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "book and premium subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `"coin":"BTC","type":"bbo"`) && strings.Contains(subs, `"coin":"BTC","type":"activeAssetCtx"`) &&
			strings.Contains(subs, `"coin":"BTC","fast":false,"type":"l2Book"`)
	})
	clockMs.Store(B - 15*60_000)
	fs.broadcast(t, l2TestAck("BTC", nil))
	waitFor(t, "book acknowledgement", func() bool {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.books[l2BookKeyFor("BTC")].Covered
	})
	frames := func() uint64 {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		p := owner.premiums[premiumKeyFor("BTC")]
		return owner.books[l2BookKeyFor("BTC")].Stats.Frames + p.Stats.QuoteFrames + p.Stats.CtxFrames
	}
	tag := 0
	for t0 := B - 14*60_000; t0 <= B; t0 += 5_000 {
		before := frames()
		mid := 100 + 0.01*float64((t0/5_000)%3)
		clockMs.Store(t0)
		fs.broadcast(t, map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": "BTC", "ctx": map[string]any{
			"oraclePx": pprTestOracle("BTC"), "markPx": pprDec(mid), "midPx": pprDec(mid), "openInterest": "5000.5"}}})
		fs.broadcast(t, map[string]any{"channel": "bbo", "data": map[string]any{"coin": "BTC", "time": t0 - 200,
			"bbo": []any{pprLevel(pprDec(mid-0.005), "1.5", 3), pprLevel(pprDec(mid+0.005), "2.25", 4)}}})
		fs.broadcast(t, l2TestBook("BTC", t0-300, tag))
		tag++
		waitFor(t, fmt.Sprintf("frames at %d", t0), func() bool { return frames() >= before+3 })
		owner.AdvancePremiums(clock())
	}
	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-mix", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{60}, time.UnixMilli(B-3_600_000))
	sealer.startServing(time.UnixMilli(B - 3_600_000))
	key := B / 1000
	clockMs.Store(B + 1_500)
	owner.AdvancePremiums(clock())
	sealer.sealOne(ctx, key)
	blob := append([]byte(nil), sealer.sealBytes(key)...)
	if len(blob) == 0 {
		t.Fatalf("no seal for key %d: %+v", key, sealer.lookup(key))
	}
	doc, err := decodeFeedSeal(blob, key)
	if err != nil {
		t.Fatalf("decode the combined seal: %v", err)
	}
	if doc.V != feedSealVersionPremium || len(doc.L2Books) != 1 || len(doc.Premiums) != 1 {
		t.Fatalf("a seal with l2_books and perp_premiums must be v%d and keep both sections: v%d books=%d premiums=%d",
			feedSealVersionPremium, doc.V, len(doc.L2Books), len(doc.Premiums))
	}
	for name, mutate := range map[string]func(*feedSealDoc){
		"relabeled as the book version": func(d *feedSealDoc) { d.V = feedSealVersionBooks },
		"premium section stripped":      func(d *feedSealDoc) { d.Premiums = nil },
	} {
		cp, _ := decodeFeedSeal(blob, key)
		mutate(cp)
		if enc, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(enc, key); err == nil {
				t.Fatalf("a combined seal with its %s decoded", name)
			}
		}
	}
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: doc.snapshot(), SharedKey: key}
	bp := oiTestPayload(t, feed, book)
	pp := oiTestPayload(t, feed, prem)
	if bp.Version != marketSnapshotVersion || pp.Version != marketSnapshotVersion || marketSnapshotVersion != 4 {
		t.Fatalf("both payloads must declare payload version 4: book v%d premium v%d const %d", bp.Version, pp.Version, marketSnapshotVersion)
	}
	if _, ok := bp.L2Books["BTC|l2_book"]; !ok || bp.Premiums != nil {
		t.Fatalf("the book strategy payload must carry its l2_books section and no premium section: books=%v premiums=%v", bp.L2Books, bp.Premiums)
	}
	if p, ok := pp.Premiums["BTC|perp_premium"]; !ok || len(p.Samples) == 0 || pp.L2Books != nil {
		t.Fatalf("the premium strategy payload must carry its perp_premiums section and no book section: premiums=%v books=%v", pp.Premiums, pp.L2Books)
	}
}
