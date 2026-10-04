package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const oiTestIntervalMs = int64(300_000)

type oiTestBar struct {
	open, high, low, close, volume float64
}

func oiTestBarAt(openMs, boundaryMs, clockMs int64) oiTestBar {
	switch {
	case openMs == boundaryMs-oiTestIntervalMs:
		return oiTestBar{100, 110.5, 99.5, 110, 50}
	case openMs == boundaryMs && clockMs < boundaryMs+oiTestIntervalMs:
		return oiTestBar{110, 110.2, 109.8, 110, 5}
	case openMs == boundaryMs:
		return oiTestBar{110, 110.2, 94.5, 95, 60}
	case openMs > boundaryMs:
		return oiTestBar{95, 95.2, 94.8, 95, 5}
	}
	return oiTestBar{100, 100.5, 99.5, 100, 10}
}

func oiTestCandles(boundaryMs, clockMs int64) []hlCandleRaw {
	last := clockMs / oiTestIntervalMs * oiTestIntervalMs
	out := make([]hlCandleRaw, 0, 260)
	for i := int64(259); i >= 0; i-- {
		open := last - i*oiTestIntervalMs
		b := oiTestBarAt(open, boundaryMs, clockMs)
		out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + oiTestIntervalMs - 1, HasClose: true,
			Open: b.open, High: b.high, Low: b.low, Close: b.close, Volume: b.volume})
	}
	return out
}

func oiTestStrategy(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   hyperliquidCheckScript,
		Args:     []string{"donchian_breakout", coin, "5m", "--mode=paper"},
		OpenStrategy: StrategyRef{
			Name:   openInterestBreakoutStrategyName,
			Params: map[string]interface{}{"oi_lookback": float64(4)},
		},
	}
}

func TestOpenInterestObservationPipeline(t *testing.T) {
	boundary := (time.Now().UTC().UnixMilli()/oiTestIntervalMs)*oiTestIntervalMs - 2*oiTestIntervalMs
	var clockMs atomic.Int64
	clockMs.Store(boundary - 30*60_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }

	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		return oiTestCandles(boundary, clockMs.Load()), nil
	})

	btc := oiTestStrategy("oi-btc", "BTC")
	eth := oiTestStrategy("oi-eth", "ETH")
	cfg := &Config{Strategies: []StrategyConfig{btc, eth}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	btcKey, ethKey := openInterestKeyFor("BTC"), openInterestKeyFor("ETH")
	wantWindow := 4*oiTestIntervalMs + openInterestDefaultMaxAgeMs + feedObservationCadenceMs
	if req.Observations[btcKey] != wantWindow || req.Observations[ethKey] != wantWindow {
		t.Fatalf("observation requirement %+v, want %dms for BTC and ETH", req.Observations, wantWindow)
	}
	if !req.Strategies["oi-btc"].OpenInterest {
		t.Fatalf("strategy requirement does not declare open interest: %+v", req.Strategies["oi-btc"])
	}

	bad := oiTestStrategy("oi-bad", "BTC")
	bad.OpenStrategy.Params = map[string]interface{}{"oi_lookback": -1.0}
	if _, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{bad}}); err == nil || !strings.Contains(err.Error(), "oi_lookback") {
		t.Fatalf("a negative oi_lookback must fail preflight, got %v", err)
	}
	restCfg := &Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{btc}}
	if err := validateMarketFeedConfig(restCfg); err == nil || !strings.Contains(err.Error(), "cannot supply open-interest observations") {
		t.Fatalf("market_feed=rest with an open-interest strategy: err %v, want a preflight refusal", err)
	}
	plain, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{{
		ID: "plain", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"donchian_breakout", "BTC", "5m", "--mode=paper"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	served := skipObservationConsumers(feedSourceREST, []feedConsumer{{Path: "oi", Loaded: true, Req: req}, {Path: "plain", Loaded: true, Req: plain}})
	if served[0].Loaded || !strings.Contains(served[0].Err, "cannot collect observations") || !served[1].Loaded {
		t.Fatalf("a REST feed source must skip only the consumer that needs observations: %+v", served)
	}
	if union := unionFeedRequirements(served); len(union.Observations) != 0 || len(union.Order) != 1 {
		t.Fatalf("the REST union must keep the other consumer and carry no observation: %+v", union)
	}
	if kept := skipObservationConsumers(feedSourceWebsocket, []feedConsumer{{Path: "oi", Loaded: true, Req: req}}); !kept[0].Loaded {
		t.Fatalf("a websocket feed source must serve an observation consumer")
	}

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
		RunID: "harness", Coins: []string{"BTC", "ETH"}, Segment: 10 * time.Minute, WSURL: fs.url(), StartedAt: clock(),
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
	waitFor(t, "activeAssetCtx subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `"type":"activeAssetCtx"`) && strings.Contains(subs, `"coin":"ETH"`)
	})

	received := func() uint64 {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.observations[btcKey].Stats.Received
	}
	sendRaw := func(atMs int64, oi any) {
		t.Helper()
		before := received()
		clockMs.Store(atMs)
		fs.broadcast(t, map[string]any{
			"channel": "activeAssetCtx",
			"data": map[string]any{"coin": "BTC", "ctx": map[string]any{
				"funding": "0.0000125", "openInterest": oi, "markPx": "100.0", "oraclePx": "100.0",
				"prevDayPx": "100.0", "dayNtlVlm": "1.0",
			}},
		})
		waitFor(t, fmt.Sprintf("ingest at %d", atMs), func() bool { return received() > before })
	}
	oiAt := func(atMs int64) string {
		minutes := float64(atMs-(boundary-30*60_000)) / 60_000
		return fmt.Sprintf("%.5f", 1000*(1+0.0005*minutes))
	}
	send := func(atMs int64) { sendRaw(atMs, oiAt(atMs)) }
	sendCandle := func(openMs int64) {
		b := oiTestBarAt(openMs, boundary, clockMs.Load())
		fs.broadcast(t, map[string]any{"channel": "candle", "data": map[string]any{
			"t": openMs, "T": openMs + oiTestIntervalMs - 1, "s": "BTC", "i": "5m",
			"o": fmt.Sprint(b.open), "h": fmt.Sprint(b.high), "l": fmt.Sprint(b.low), "c": fmt.Sprint(b.close), "v": fmt.Sprint(b.volume),
		}})
	}
	reconnect := func(resumeAt int64) {
		t.Helper()
		sessions := fs.sessionCount()
		fs.dropConnections()
		waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
		waitFor(t, "resubscribe", func() bool {
			return strings.Count(strings.Join(fs.subscriptions(), " "), `"coin":"BTC","type":"activeAssetCtx"`) > sessions
		})
		send(resumeAt)
		owner.feedMu.Lock()
		gaps := append([]feedObservationGap(nil), owner.observations[btcKey].Gaps...)
		owner.feedMu.Unlock()
		if n := len(gaps); n == 0 || gaps[n-1].Reason != feedObservationGapDisconnected || gaps[n-1].EndMs != resumeAt {
			t.Fatalf("the reconnect gap marker was not closed by the first sample of the new session: %+v", gaps)
		}
	}

	start := boundary - 30*60_000
	for at := start; at <= boundary-27*60_000; at += 10_000 {
		send(at)
	}
	reconnect(boundary - 26*60_000 - 50_000)
	for at := boundary - 26*60_000 - 40_000; at <= boundary-15*60_000; at += 10_000 {
		send(at)
	}
	sendRaw(boundary-15*60_000-30_000, oiAt(boundary-15*60_000))
	sendRaw(boundary-15*60_000+5_000, "abc")
	sendRaw(boundary-15*60_000+6_000, "-5")
	send(boundary - 15*60_000 + 7_000)
	send(boundary - 15*60_000 + 7_000)
	for at := boundary - 15*60_000 + 10_000; at <= boundary; at += 10_000 {
		send(at)
	}
	send(boundary + 10_000)

	owner.feedMu.Lock()
	st := owner.observations[btcKey]
	stats := st.Stats
	firstRecv := st.Samples[0].RecvAtMs
	nSamples := len(st.Samples)
	retainedGaps := len(st.Gaps)
	owner.feedMu.Unlock()
	if stats.Rejected != 3 || !strings.Contains(stats.LastReject, "negative") && !strings.Contains(stats.LastReject, "clock") {
		t.Fatalf("rejected %d (last %q), want 3: clock regression, malformed and negative", stats.Rejected, stats.LastReject)
	}
	if stats.Refreshed == 0 {
		t.Fatalf("same-bucket refreshes were not counted: %+v", stats)
	}
	if retain := st.retainMs(); firstRecv < boundary+10_000-retain || nSamples > int(retain/feedObservationCadenceMs)+1 {
		t.Fatalf("bounded storage: first sample %d, %d samples, retention %dms", firstRecv, nSamples, retain)
	}
	health := owner.Health("")
	if len(health.Observations) != 2 || health.Observations[0].Key != "BTC|open_interest" ||
		health.Observations[0].Rejected != 3 || health.Observations[1].Status != feedObservationStatusEmpty {
		t.Fatalf("feed health must report each observation key with its counters: %+v", health.Observations)
	}
	if retainedGaps != 0 {
		t.Fatalf("a gap marker older than the retention window was kept (%d gaps)", retainedGaps)
	}

	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-oi", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{300}, time.UnixMilli(boundary-3_600_000))
	sealer.startServing(time.UnixMilli(boundary - 3_600_000))
	sockDir, err := os.MkdirTemp("", "oi")
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

	key1 := boundary / 1000
	sealer.sealOne(ctx, key1)
	sealed1 := append([]byte(nil), sealer.sealBytes(key1)...)
	if len(sealed1) == 0 {
		t.Fatalf("no seal for key %d: %+v", key1, sealer.lookup(key1))
	}
	doc, err := decodeFeedSeal(sealed1, key1)
	if err != nil {
		t.Fatalf("decode seal: %v", err)
	}
	if doc.V != 2 || len(doc.Observations) != 2 {
		t.Fatalf("seal v%d carries %d observation entries, want v2 with BTC and ETH", doc.V, len(doc.Observations))
	}
	for _, smp := range doc.Observations[0].Samples {
		if smp.RecvAtMs > boundary {
			t.Fatalf("a sample received after the cutoff entered the seal: %+v", smp)
		}
	}

	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, time.Hour
	descs := client.Describe(ctx)
	if descs[0].Err != nil || len(descs[0].Describe.Observations) != 2 {
		t.Fatalf("describe: %+v", descs[0])
	}
	_, problems := sharedFeedCompatibilityLines("test", descs, req, []int{300})
	if len(problems) != 0 {
		t.Fatalf("a feed serving the observation windows reported coverage problems: %v", problems)
	}
	wider := req
	wider.Observations = map[feedObservationKey]int64{btcKey: wantWindow * 2}
	if _, problems := sharedFeedCompatibilityLines("test", descs, wider, []int{300}); len(problems) == 0 || !strings.Contains(problems[0], "BTC|open_interest window") {
		t.Fatalf("a consumer needing a longer window must see a coverage gap, got %v", problems)
	}

	due := []StrategyConfig{btc, eth}
	reqs := cycleRequirementsForDue(due, req)
	snap1, rep := client.Fetch(ctx, key1, reqs)
	if rep.Status != feedFetchSealed || len(rep.Gaps) != 0 {
		t.Fatalf("fetch key %d: %+v", key1, rep)
	}
	feed1 := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap1, SharedKey: key1}
	if hold := feed1.holdFor(btc); hold.Held {
		t.Fatalf("an open-interest strategy with a ready candle frame was held: %+v", hold)
	}
	payload1 := oiTestPayload(t, feed1, btc)
	ethPayload := oiTestPayload(t, feed1, eth)
	if ob := payload1.Observations["BTC|open_interest"]; !ob.Available || ob.CutoffMs != boundary || len(ob.Samples) == 0 || ob.BarIntervalMs != oiTestIntervalMs || ob.BarOffsetMs != 1 {
		t.Fatalf("BTC payload observation: %+v", ob)
	}
	if ob := ethPayload.Observations["ETH|open_interest"]; !ob.Available || len(ob.Samples) != 0 || ob.FeedStatus != feedObservationStatusEmpty {
		t.Fatalf("ETH payload observation (no samples recorded): %+v", ob)
	}
	if _, ok := ethPayload.Frames["ETH|5m"]; !ok {
		t.Fatalf("the ETH candle frame must stay in the payload while its open-interest window is empty")
	}
	missing := snap1.observationPayload(openInterestKeyFor("SOL"), wantWindow, oiTestIntervalMs)
	if missing.Available || !strings.Contains(missing.Reason, "not in the sealed snapshot") {
		t.Fatalf("an unsealed observation key must be unavailable with a reason: %+v", missing)
	}

	send(boundary + 20_000)
	sealer.sealOne(ctx, key1)
	if !bytes.Equal(sealed1, sealer.sealBytes(key1)) {
		t.Fatal("a later arrival or a second seal attempt changed immutable seal bytes")
	}

	for at := boundary + 30_000; at <= boundary+60_000; at += 10_000 {
		send(at)
	}
	reconnect(boundary + 90_000)
	for at := boundary + 100_000; at <= boundary+oiTestIntervalMs; at += 10_000 {
		send(at)
	}
	clockMs.Store(boundary + oiTestIntervalMs + 2_000)
	sendCandle(boundary)
	sendCandle(boundary + oiTestIntervalMs)
	waitFor(t, "closed candle", func() bool {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		bars := owner.keys[feedKeyFor("BTC", "5m")].Bars
		return len(bars) > 1 && bars[len(bars)-1].OpenMs == boundary+oiTestIntervalMs && bars[len(bars)-2].Close == 95
	})
	key2 := (boundary + oiTestIntervalMs) / 1000
	sealer.sealOne(ctx, key2)
	snap2, rep2 := client.Fetch(ctx, key2, reqs)
	if rep2.Status != feedFetchSealed {
		t.Fatalf("fetch key %d: %+v", key2, rep2)
	}
	payload2 := oiTestPayload(t, &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap2, SharedKey: key2}, btc)
	ob2 := payload2.Observations["BTC|open_interest"]
	var gapInWindow bool
	for _, g := range ob2.Gaps {
		gapInWindow = gapInWindow || (g.Reason == feedObservationGapDisconnected && g.EndMs != nil && *g.EndMs == boundary+90_000)
	}
	if !ob2.Available || !gapInWindow {
		t.Fatalf("the second payload must carry the in-window disconnect gap: %+v", ob2.Gaps)
	}
	for _, g := range ob2.Gaps {
		if g.DetectedMs < g.StartMs || g.DetectedMs > ob2.CutoffMs {
			t.Fatalf("a sealed gap must carry a detection time between its start and the cutoff: %+v", g)
		}
	}
	late := newFeedObservationState(btcKey, wantWindow)
	late.ingest(1000, 0, boundary-60_000, 1)
	late.markDisconnected(boundary + 30_000)
	if frozen := freezeObservation(late, wantWindow, boundary, boundary+40_000); len(frozen.Gaps) != 0 {
		t.Fatalf("a gap detected after the cutoff must not enter a seal for that cutoff: %+v", frozen.Gaps)
	}
	if frozen := freezeObservation(late, wantWindow, boundary+30_000, boundary+40_000); len(frozen.Gaps) != 1 || frozen.Gaps[0].StartMs != boundary-60_000 {
		t.Fatalf("a gap detected at the cutoff starts at the last sample: %+v", frozen.Gaps)
	}

	oldSeal := oiTestOldPeer(t, sockDir)
	oldClient := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "old", Socket: oldSeal}})
	if _, _, err := oldClient.roundTrip(ctx, oldSeal, feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe}); err == nil || !strings.Contains(err.Error(), feedErrIncompatible) {
		t.Fatalf("a seal-v1 peer must be rejected as incompatible, got %v", err)
	}

	cancel()
	waitFor(t, "disconnect", func() bool { return !owner.Connected() })
	owner.attachRecorder(nil)
	stopRec()
	if err := rec.wait(); err != nil {
		t.Fatalf("recorder: %v", err)
	}
	if !rec.manifest.Closed || len(rec.manifest.Segments) == 0 || rec.manifest.DroppedTotal != 0 {
		t.Fatalf("recorder manifest: closed=%t segments=%d dropped=%d", rec.manifest.Closed, len(rec.manifest.Segments), rec.manifest.DroppedTotal)
	}

	if out := os.Getenv("GO_TRADER_OBS_HARNESS_OUT"); out != "" {
		oiTestWriteHarness(t, out, recDir, payload1, payload2)
	}
}

func oiTestPayload(t *testing.T, feed *marketFeedContext, sc StrategyConfig) *marketPayload {
	t.Helper()
	blob, err := feed.singleCheckPayload(sc)
	if err != nil {
		t.Fatalf("single check payload for %s: %v", sc.ID, err)
	}
	var env struct {
		V      int            `json:"v"`
		Market *marketPayload `json:"market"`
	}
	if err := json.Unmarshal(blob, &env); err != nil || env.Market == nil {
		t.Fatalf("decode stdin envelope: %v", err)
	}
	return env.Market
}

func oiTestOldPeer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "old.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = readFeedFrame(conn, feedWireMaxRequestBytes)
			_ = writeFeedJSONFrame(conn, map[string]any{
				"v": feedWireVersion, "status": feedWireStatusDescribe, "instance": "old", "source": feedSourceWebsocket,
				"generation": 1, "seal_version": 1, "payload_version": marketSnapshotVersion,
			})
			conn.Close()
		}
	}()
	return path
}

func oiTestWriteHarness(t *testing.T, out, recDir string, payloads ...*marketPayload) {
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
	type harnessCase struct {
		Payload string         `json:"payload"`
		Expect  map[string]any `json:"expect"`
	}
	cases := []harnessCase{
		{Payload: "payload_1.json", Expect: map[string]any{"last_signal": 1, "last_reason": "entry_long", "last_carried": true}},
		{Payload: "payload_2.json", Expect: map[string]any{"last_signal": 0, "last_reason": "observation_gap", "last_carried": true}},
	}
	for i, p := range payloads {
		blob, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, cases[i].Payload), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	harness := map[string]any{
		"coin": "BTC", "timeframe": "5m", "params": map[string]any{"oi_lookback": 4},
		"expected_rejected_records": 3, "cases": cases,
	}
	blob, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "harness.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}
