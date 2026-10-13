package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tfTestBarMs = int64(300_000)

func tfTestStrategy(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   hyperliquidCheckScript,
		Args:     []string{"donchian_breakout", coin, "5m", "--mode=paper"},
		OpenStrategy: StrategyRef{
			Name:   cumulativeVolumeDeltaStrategyName,
			Params: map[string]interface{}{"session_hours": float64(1)},
		},
	}
}

type tfTestTrade struct {
	Coin   string
	Tid    uint64
	TimeMs int64
	Side   string
	Sz     string
}

func (tr tfTestTrade) wire() map[string]any {
	return map[string]any{
		"coin": tr.Coin, "side": tr.Side, "px": "100.50", "sz": tr.Sz, "time": tr.TimeMs,
		"hash": "0xAB" + fmt.Sprintf("%062x", tr.Tid), "tid": tr.Tid,
		"users": []string{"0xBuyer", "0xSeller"},
	}
}

func tfScheduledTrade(coin string, eventMs int64) tfTestTrade {
	step := eventMs / 10_000
	side := "B"
	if step%3 == 0 {
		side = "A"
	}
	tid := uint64(eventMs / 1000)
	if coin == "ETH" {
		tid += 1_000_000_000_000
	}
	return tfTestTrade{Coin: coin, Tid: tid, TimeMs: eventMs, Side: side, Sz: fmt.Sprintf("0.%03d", 1+step%7)}
}

func tfScheduledBarVolume(openMs, startMs, endMs int64) float64 {
	var units int64
	for e := openMs + 5_000; e < openMs+tfTestBarMs; e += 10_000 {
		if e < startMs || e >= endMs {
			continue
		}
		u, _ := feedDecimalUnits(tfScheduledTrade("BTC", e).Sz)
		units += u
	}
	if units == 0 {
		return 10
	}
	return float64(units) / float64(feedTradeScale)
}

func TestTradeFlowObservationPipeline(t *testing.T) {
	hour := int64(3_600_000)
	B := (time.Now().UTC().UnixMilli()/hour)*hour - 3*hour
	tradeEnd := B + 70*60_000
	var clockMs atomic.Int64
	clockMs.Store(B - 20*60_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }

	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		last := clockMs.Load()/tfTestBarMs*tfTestBarMs - tfTestBarMs
		out := make([]hlCandleRaw, 0, 300)
		for i := int64(279); i >= 0; i-- {
			open := last - i*tfTestBarMs
			v := 10.0
			if coin == "BTC" {
				v = tfScheduledBarVolume(open, B, tradeEnd)
			}
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + tfTestBarMs - 1, HasClose: true,
				Open: 100, High: 101, Low: 99, Close: 100.5, Volume: v})
		}
		return out, nil
	})

	btc := tfTestStrategy("cvd-btc", "BTC")
	eth := tfTestStrategy("cvd-eth", "ETH")
	ethOI := oiTestStrategy("oi-eth", "ETH")
	ethOI.Args[2] = "5m"
	cfg := &Config{Strategies: []StrategyConfig{btc, eth, ethOI}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	btcKey, ethKey, ethOIKey := tradeFlowKeyFor("BTC"), tradeFlowKeyFor("ETH"), openInterestKeyFor("ETH")
	wantWindow := hour + tfTestBarMs + feedTradeIntervalMs
	if req.Observations[btcKey] != wantWindow || req.Observations[ethKey] != wantWindow || req.Observations[ethOIKey] == 0 {
		t.Fatalf("observation requirements %+v, want trade windows %dms and an ETH open-interest window", req.Observations, wantWindow)
	}
	if e := req.Strategies["cvd-btc"]; !e.TradeFlow || e.OpenInterest || e.TradeWindowMs != wantWindow {
		t.Fatalf("strategy requirement %+v", e)
	}
	bad := tfTestStrategy("cvd-bad", "BTC")
	bad.OpenStrategy.Params = map[string]interface{}{"session_hours": float64(5)}
	if _, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{bad}}); err == nil || !strings.Contains(err.Error(), "does not divide 24") {
		t.Fatalf("a session that does not divide the day must fail preflight, got %v", err)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{btc}}); err == nil || !strings.Contains(err.Error(), "cannot supply trade-flow observations") {
		t.Fatalf("market_feed=rest with a trade-flow strategy: %v", err)
	}
	served := dropUncollectableObservations(feedSourceREST, []feedConsumer{{Path: "cvd", Loaded: true, Req: req}})
	if !served[0].Loaded || len(served[0].Req.Observations) != 0 || served[0].Req.Strategies["cvd-btc"].TradeFlow || len(served[0].Req.Order) != len(req.Order) {
		t.Fatalf("a REST source must drop only the trade and open-interest needs: %+v", served[0].Req)
	}
	union := unionFeedRequirements([]feedConsumer{{Path: "cvd", Loaded: true, Req: req}})
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
		RunID: "trade-harness", Coins: []string{"BTC", "ETH"}, Kinds: []string{feedObservationTradeFlow, feedObservationOpenInterest},
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
	waitFor(t, "trades subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `"coin":"BTC","type":"trades"`) && strings.Contains(subs, `"coin":"ETH","type":"trades"`) &&
			strings.Contains(subs, `"coin":"ETH","type":"activeAssetCtx"`)
	})

	state := func(key feedObservationKey) feedTradeFlowState {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		st := owner.tradeFlows[key]
		cp := *st
		cp.Intervals = nil
		for _, iv := range st.Intervals {
			c := *iv
			cp.Intervals = append(cp.Intervals, &c)
		}
		cp.Gaps = append([]feedObservationGap(nil), st.Gaps...)
		return cp
	}
	frames := func(coin string) uint64 { return state(tradeFlowKeyFor(coin)).Stats.Frames }
	sendFrame := func(coin string, recvMs int64, trades ...map[string]any) {
		t.Helper()
		before := frames(coin)
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "trades", "data": trades})
		waitFor(t, fmt.Sprintf("%s frame at %d", coin, recvMs), func() bool { return frames(coin) > before })
	}
	ack := func(coin string, recvMs int64) {
		t.Helper()
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "subscriptionResponse", "data": map[string]any{
			"method": "subscribe", "subscription": map[string]any{"type": "trades", "coin": coin}}})
		waitFor(t, coin+" coverage start", func() bool { return state(tradeFlowKeyFor(coin)).CoverFromMs != 0 })
	}
	oiReceived := func() uint64 {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.observations[ethOIKey].Stats.Received
	}
	sendOI := func(recvMs int64) {
		t.Helper()
		before := oiReceived()
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": "ETH", "ctx": map[string]any{
			"openInterest": fmt.Sprintf("%.3f", 5000+float64(recvMs-B)/1e6)}}})
		waitFor(t, "ETH open interest", func() bool { return oiReceived() > before })
	}

	sendFrame("BTC", B-25_000, tfScheduledTrade("BTC", B-40_000).wire())
	ack("BTC", B-20_000)
	ack("ETH", B-19_000)
	if st := state(btcKey); st.CoverFromMs != B || st.Stats.Uncovered != 1 {
		t.Fatalf("coverage must start at the first minute after the acknowledgement and a pre-subscription trade is uncovered: from %d, %+v", st.CoverFromMs, st.Stats)
	}

	var sealed1 []byte
	key1 := (B + 30*60_000) / 1000
	key2 := (B + 65*60_000) / 1000
	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-tf", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{300}, time.UnixMilli(B-3_600_000))
	sealer.startServing(time.UnixMilli(B - 3_600_000))
	sockDir, err := os.MkdirTemp("", "tf")
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

	reconnected := false
	for k := int64(0); k < 70; k++ {
		start := B + k*60_000
		for off := int64(5_000); off < 60_000; off += 10_000 {
			e := start + off
			recv := e + 300
			if k == 50 && off > 15_000 {
				continue
			}
			if k == 58 && off == 25_000 && !reconnected {
				clockMs.Store(e)
				sessions := fs.sessionCount()
				fs.dropConnections()
				waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
				waitFor(t, "resubscribe", func() bool {
					return strings.Count(strings.Join(fs.subscriptions(), " "), `"coin":"BTC","type":"trades"`) > 1
				})
				if st := state(btcKey); st.CoverFromMs != 0 || len(st.Gaps) == 0 || st.Gaps[len(st.Gaps)-1].Reason != feedTradeGapDisconnected {
					t.Fatalf("a disconnect must stop coverage with a dated gap: %+v", st.Gaps)
				}
				ack("BTC", e+1_000)
				snapshot := []map[string]any{tfScheduledTrade("BTC", B+57*60_000+45_000).wire(),
					{"coin": "BTC", "side": "B", "px": "100.50", "sz": "0.5", "time": B + 57*60_000 + 59_000, "hash": "0x01", "tid": 777_000_001, "users": []string{"0xa", "0xb"}}}
				sendFrame("BTC", e+1_500, snapshot...)
				reconnected = true
				continue
			}
			trades := []map[string]any{tfScheduledTrade("BTC", e).wire()}
			switch {
			case k == 5 && off == 25_000:
				for j := int64(0); j < 2000; j++ {
					trades = append(trades, tfTestTrade{Coin: "BTC", Tid: uint64(900_000_000 + j), TimeMs: start + 20_000 + j%30_000/1000, Side: "B", Sz: "0.001"}.wire())
				}
			case k == 10 && off == 35_000:
				trades = append(trades, tfScheduledTrade("BTC", e-10_000).wire())
			case k == 20 && off == 45_000:
				trades = nil
			case k == 20 && off == 55_000:
				trades = append(trades, tfScheduledTrade("BTC", e-10_000).wire())
			case k == 25 && off == 25_000:
				conflict := tfScheduledTrade("BTC", e-10_000)
				conflict.Sz = "9.999"
				trades = append(trades, conflict.wire())
			case k == 30 && off == 15_000:
				trades = append(trades, tfTestTrade{Coin: "BTC", Tid: 888_000_001, TimeMs: B + 29*60_000 + 59_000, Side: "A", Sz: "0.2"}.wire())
			case k == 35 && off == 25_000:
				trades = append(trades, map[string]any{"coin": "BTC", "side": "B", "px": "100", "sz": "abc", "time": e, "hash": "0x", "tid": 777_000_002, "users": []string{"0xa", "0xb"}})
			case k == 40 && off == 25_000:
				trades = append(trades, tfTestTrade{Coin: "BTC", Tid: 777_000_003, TimeMs: recv + 10_000, Side: "B", Sz: "0.1"}.wire())
			case k == 45 && off == 25_000:
				sendFrame("BTC", recv-15_000, tfTestTrade{Coin: "BTC", Tid: 777_000_004, TimeMs: recv - 15_500, Side: "B", Sz: "0.1"}.wire())
			}
			if trades != nil {
				sendFrame("BTC", recv, trades...)
			}
			if k < 10 {
				sendFrame("ETH", recv+50, tfScheduledTrade("ETH", e).wire())
			}
			if off == 5_000 {
				sendOI(recv + 100)
			}
			if k == 50 && off == 15_000 {
				clockMs.Store(start + 50_000)
				owner.AdvanceTradeFlows(clock())
				if st := state(btcKey); st.CoverFromMs != 0 || !st.Resumable || st.Gaps[len(st.Gaps)-1].Reason != feedTradeGapSilence {
					t.Fatalf("a 30 s frame silence must stop coverage and allow a resume: %+v", st)
				}
			}
			if k == 30 && off == 15_000 {
				clockMs.Store(recv + 5_000)
				sealer.sealOne(ctx, key1)
				sealed1 = append([]byte(nil), sealer.sealBytes(key1)...)
				if len(sealed1) == 0 {
					t.Fatalf("no seal for key %d: %+v", key1, sealer.lookup(key1))
				}
			}
		}
		if k == 65 {
			clockMs.Store(B + 65*60_000 + 1_000)
			sealer.sealOne(ctx, key2)
			if len(sealer.sealBytes(key2)) == 0 {
				t.Fatalf("no seal for key %d: %+v", key2, sealer.lookup(key2))
			}
		}
	}

	st := state(btcKey)
	byStart := map[int64]*feedTradeInterval{}
	for _, iv := range st.Intervals {
		byStart[iv.StartMs] = iv
	}
	at := func(k int64) *feedTradeInterval { return byStart[B+k*60_000] }
	expect := func(k int64, status, reason string) {
		t.Helper()
		iv := at(k)
		if iv == nil {
			t.Fatalf("interval %d is missing", k)
		}
		got := iv.Status
		if iv.InvalidMs != 0 {
			got = feedTradeStatusInvalid
		}
		if got != status || iv.Reason != reason {
			t.Fatalf("interval %d: status %s reason %q, want %s %q (%+v)", k, got, iv.Reason, status, reason, iv)
		}
	}
	expect(1, feedTradeStatusComplete, "")
	if iv := at(5); iv.Trades != 2006 {
		t.Fatalf("the high-volume interval tallies %d trades, want 2006", iv.Trades)
	}
	expect(10, feedTradeStatusComplete, "")
	if at(10).Trades != 6 || st.Stats.Duplicates != 2 {
		t.Fatalf("an identical duplicate must be idempotent: tally %d, duplicates %d", at(10).Trades, st.Stats.Duplicates)
	}
	expect(20, feedTradeStatusComplete, "")
	if at(20).Trades != 6 {
		t.Fatalf("an in-grace out-of-order trade must count once: %d", at(20).Trades)
	}
	expect(25, feedTradeStatusInvalid, "conflicting_duplicate")
	expect(29, feedTradeStatusInvalid, "late_event")
	if at(29).FinalizedMs >= at(29).InvalidMs {
		t.Fatalf("the late trade must revoke interval 29 after it became final: %+v", at(29))
	}
	expect(35, feedTradeStatusInvalid, feedTradeGapMalformed)
	expect(40, feedTradeStatusInvalid, "future_event")
	expect(45, feedTradeStatusInvalid, feedTradeGapClock)
	expect(50, feedTradeStatusInvalid, feedTradeGapSilence)
	if at(51) != nil {
		t.Fatalf("the minute that resumed coverage must stay unknown: %+v", at(51))
	}
	expect(52, feedTradeStatusComplete, "")
	expect(57, feedTradeStatusInvalid, "late_event")
	expect(58, feedTradeStatusInvalid, feedTradeGapDisconnected)
	expect(59, feedTradeStatusComplete, "")
	if st.Stats.Conflicts != 1 || st.Stats.Rejected != 3 || st.Stats.Late < 2 {
		t.Fatalf("outcome counters %+v", st.Stats)
	}
	ethState := state(ethKey)
	if ethState.CoverFromMs != 0 || ethState.Gaps[len(ethState.Gaps)-1].Reason != feedTradeGapSilence {
		t.Fatalf("ETH trades stopped after 10 minutes; coverage must end on silence: %+v", ethState.Gaps)
	}

	doc1, err := decodeFeedSeal(sealed1, key1)
	if err != nil {
		t.Fatalf("decode seal 1: %v", err)
	}
	if doc1.V != feedSealVersionTrades || len(doc1.TradeFlows) != 2 || len(doc1.Observations) != 1 {
		t.Fatalf("seal 1 v%d carries %d trade flows and %d open-interest entries", doc1.V, len(doc1.TradeFlows), len(doc1.Observations))
	}
	for _, iv := range doc1.TradeFlows[0].Intervals {
		if iv.FinalizedMs > key1*1000 || iv.StartMs >= B+29*60_000 {
			t.Fatalf("seal 1 holds an interval that was not final by its cutoff: %+v", iv)
		}
	}
	sealer.sealOne(ctx, key1)
	if !bytes.Equal(sealed1, sealer.sealBytes(key1)) {
		t.Fatal("later trades or a second seal attempt changed immutable seal bytes")
	}
	for _, mutate := range []func(*feedSealDoc){
		func(d *feedSealDoc) { d.V = feedSealVersionAccounting },
		func(d *feedSealDoc) { d.TradeFlows = nil },
		func(d *feedSealDoc) { d.TradeFlows[0].SideEvidence = "unverified" },
		func(d *feedSealDoc) { d.TradeFlows[0].Intervals[0].Trades = 0 },
		func(d *feedSealDoc) { d.TradeFlows[0].Intervals[0].Buy = d.TradeFlows[0].Intervals[0].Buy + "0" },
	} {
		cp, _ := decodeFeedSeal(sealed1, key1)
		mutate(cp)
		if blob, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(blob, key1); err == nil {
				t.Fatalf("a tampered or relabeled trade seal decoded: v%d", cp.V)
			}
		}
	}
	if len(sealed1) > estimate || len(sealed1) > feedSealMaxBytes {
		t.Fatalf("seal 1 is %d bytes; estimate %d, cap %d", len(sealed1), estimate, feedSealMaxBytes)
	}

	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, 6*time.Hour
	descs := client.Describe(ctx)
	if descs[0].Err != nil || len(descs[0].Describe.Observations) != 3 {
		t.Fatalf("describe: %+v", descs[0])
	}
	if _, problems := sharedFeedCompatibilityLines("test", descs, req, []int{300}); len(problems) != 0 {
		t.Fatalf("a feed serving the trade windows reported coverage problems: %v", problems)
	}
	wider := req
	wider.Observations = map[feedObservationKey]int64{btcKey: wantWindow * 2}
	if _, problems := sharedFeedCompatibilityLines("test", descs, wider, []int{300}); len(problems) == 0 || !strings.Contains(problems[0], "BTC|trade_flow window") {
		t.Fatalf("a consumer needing a longer trade window must see a coverage gap, got %v", problems)
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
	payload1 := fetchPayload(key1, btc)
	payload2 := fetchPayload(key2, btc)
	ethPayload := fetchPayload(key2, eth)
	ethOIPayload := fetchPayload(key2, ethOI)
	flow2 := payload2.TradeFlows["BTC|trade_flow"]
	if !flow2.Available || flow2.CutoffMs != key2*1000 || flow2.SideEvidence != feedTradeSideEvidence || flow2.BarIntervalMs != tfTestBarMs {
		t.Fatalf("BTC payload trade flow: %+v", flow2)
	}
	statuses := map[int64]string{}
	for _, iv := range flow2.Intervals {
		statuses[(iv.StartMs-B)/60_000] = iv.Status + ":" + iv.Reason
	}
	for k, want := range map[int64]string{25: "invalid:conflicting_duplicate", 29: "invalid:late_event", 57: "invalid:late_event", 59: "complete:", 1: "complete:"} {
		if statuses[k] != want {
			t.Fatalf("payload 2 interval %d is %q, want %q", k, statuses[k], want)
		}
	}
	if _, ok := statuses[51]; ok {
		t.Fatal("an unknown minute entered the payload as an interval")
	}
	if f := ethPayload.TradeFlows["ETH|trade_flow"]; !f.Available || len(f.Intervals) == 0 {
		t.Fatalf("ETH trade flow must stay available with its recorded intervals: %+v", f)
	}
	if ob := ethOIPayload.Observations["ETH|open_interest"]; !ob.Available || len(ob.Samples) == 0 {
		t.Fatalf("ETH open interest must stay ready while its trade coverage is lost: %+v", ob)
	}
	if _, ok := ethPayload.Frames["ETH|5m"]; !ok {
		t.Fatal("the ETH candle frame must stay in the payload while its trade flow is degraded")
	}
	if _, ok := ethPayload.Observations["ETH|open_interest"]; ok {
		t.Fatal("a trade-flow strategy payload must not carry another strategy's open interest")
	}
	single, _ := json.Marshal(payload2)
	if len(single) > marketPayloadMaxBytes {
		t.Fatalf("single payload %d bytes over the %d-byte cap", len(single), marketPayloadMaxBytes)
	}
	missing := (&marketSnapshot{EvaluationID: "x", tradeFlows: map[feedObservationKey]*marketSnapshotTradeFlow{}}).tradeFlowPayload(tradeFlowKeyFor("SOL"), wantWindow, tfTestBarMs)
	if missing.Available || !strings.Contains(missing.Reason, "not in the sealed snapshot") {
		t.Fatalf("an unsealed trade key must be unavailable with a reason: %+v", missing)
	}

	for _, tc := range []struct {
		sealVersion int
		compatible  bool
	}{{feedSealVersionBase, true}, {feedSealVersionTrades, true}, {feedSealVersionTrades + 1, false}} {
		peer := oiTestOldPeer(t, sockDir, tc.sealVersion)
		_, _, err := client.roundTrip(ctx, peer, feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe})
		if tc.compatible != (err == nil) {
			t.Fatalf("a seal-v%d peer: compatible=%v, err %v", tc.sealVersion, tc.compatible, err)
		}
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
	if out := os.Getenv("GO_TRADER_TRADE_HARNESS_OUT"); out != "" {
		tfWriteHarness(t, out, recDir, map[string]*marketPayload{"payload_1.json": payload1, "payload_2.json": payload2, "payload_eth.json": ethPayload})
	}
}

func TestTradeFlowCapacityAtMaximumWindow(t *testing.T) {
	window := feedTradeMaxWindowMs
	cutoff := int64(1_800_000_000_000)
	st := newFeedTradeFlowState(tradeFlowKeyFor("BTC"), window)
	for start := cutoff - window; start+feedTradeIntervalMs <= cutoff; start += feedTradeIntervalMs {
		iv := &feedTradeInterval{StartMs: start, BuyUnits: 99_999_999_999_999, SellUnits: 99_999_999_999_999, Trades: 999_999,
			Session: 12345, Status: feedTradeStatusInvalid, Reason: feedTradeGapIdentity, FinalizedMs: start + 70_000, InvalidMs: start + 70_000}
		for i := range iv.Hash {
			iv.Hash[i] = 0xff
		}
		st.Intervals = append(st.Intervals, iv)
	}
	snap := &marketSnapshot{EvaluationID: feedSealEvaluationID(cutoff / 1000), SealedAt: time.UnixMilli(cutoff), Deadline: time.UnixMilli(cutoff),
		keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{},
		observations: map[feedObservationKey]*marketSnapshotObservation{}, tradeFlows: map[feedObservationKey]*marketSnapshotTradeFlow{}}
	for _, coin := range []string{"BTC", "ETH", "SOL", "XRP"} {
		cp := *st
		cp.Key = tradeFlowKeyFor(coin)
		snap.tradeFlows[cp.Key] = freezeTradeFlow(&cp, window, cutoff, cutoff)
	}
	doc, err := feedSealDocFromSnapshot(snap, cutoff/1000, feedSourceWebsocket, "cap")
	if err != nil {
		t.Fatal(err)
	}
	blob, _, err := encodeFeedSeal(doc)
	if err != nil {
		t.Fatalf("a maximum-window four-coin trade seal must encode under the cap: %v", err)
	}
	union := feedRequirements{Observations: map[feedObservationKey]int64{}}
	for k := range snap.tradeFlows {
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
	for k := range snap.tradeFlows {
		needs[k] = window
	}
	attachObservationPayloads(payload, snap, needs, tfTestBarMs)
	batch, _ := json.Marshal(payload)
	if len(payload.TradeFlows) != 4 || len(batch) > marketPayloadMaxBytes {
		t.Fatalf("a four-coin maximum batch payload is %d bytes (cap %d), %d flows", len(batch), marketPayloadMaxBytes, len(payload.TradeFlows))
	}

	over := newFeedTradeFlowState(tradeFlowKeyFor("BTC"), window)
	var ev feedTradeEvents
	over.startCoverage(cutoff-120_000, 1, &ev)
	recv := cutoff - 60_000 + 10
	for i := 0; i <= feedTradeIdentityCap; i++ {
		over.ingestTrade(feedTrade{Coin: "BTC", Tid: uint64(i + 1), TimeMs: cutoff - 60_000 + int64(i%50_000), Side: "B", Px: "1", Sz: "0.001", Units: 100_000}, recv+int64(i%50_000), &ev)
	}
	iv := over.interval(cutoff - 60_000)
	if iv == nil || iv.InvalidMs == 0 || iv.Reason != feedTradeGapIdentity {
		t.Fatalf("an identity-store overflow must invalidate the open interval: %+v", iv)
	}
	over.LastFrameMs = recv + 60_000
	over.advance(recv+60_000, &ev)
	if len(over.idents) > feedTradeIdentityCap {
		t.Fatalf("the identity store keeps %d entries over its %d cap", len(over.idents), feedTradeIdentityCap)
	}
}

func tfWriteHarness(t *testing.T, out, recDir string, payloads map[string]*marketPayload) {
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
	names := make([]string, 0, len(payloads))
	for name, p := range payloads {
		blob, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), blob, 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	cases := []map[string]any{
		{"payload": "payload_1.json", "coin": "BTC"},
		{"payload": "payload_2.json", "coin": "BTC"},
		{"payload": "payload_eth.json", "coin": "ETH"},
	}
	harness := map[string]any{"timeframe": "5m", "params": map[string]any{"session_hours": 1, "left_span": 2, "right_span": 1, "min_separation": 2},
		"expected_rejected": map[string]int{"BTC": 3, "ETH": 0}, "cases": cases}
	blob, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "harness.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}
