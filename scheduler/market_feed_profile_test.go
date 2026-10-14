package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	vpTestDayMs = feedProfileSessionMs
	vpTestBarMs = int64(900_000)
)

var vpTestBTCCycle = []string{"100", "100", "101", "102", "102", "102", "102", "103", "104", "104", "105", "105", "105", "106", "108", "108"}

func vpTestStrategy(id, coin string) StrategyConfig {
	return StrategyConfig{
		ID:           id,
		Type:         "perps",
		Platform:     "hyperliquid",
		Script:       hyperliquidCheckScript,
		Args:         []string{"donchian_breakout", coin, "15m", "--mode=paper"},
		OpenStrategy: StrategyRef{Name: volumeProfileReclaimStrategyName},
	}
}

func vpTrade(coin string, tid uint64, timeMs int64, side, px, sz string) map[string]any {
	return map[string]any{
		"coin": coin, "side": side, "px": px, "sz": sz, "time": timeMs,
		"hash": "0x" + fmt.Sprintf("%064x", tid), "tid": tid, "users": []string{"0xBuyer", "0xSeller"},
	}
}

func vpCandleClose(coin string, openMs, d1, intervalMs int64) float64 {
	if coin == "ETH" {
		return 2005
	}
	if intervalMs == vpTestBarMs {
		switch openMs {
		case d1 + 90*60_000:
			return 101
		case d1 + 105*60_000:
			return 103
		}
	}
	return 104.5
}

func TestVolumeProfileObservationPipeline(t *testing.T) {
	D0 := (time.Now().UTC().UnixMilli()/vpTestDayMs)*vpTestDayMs - 3*vpTestDayMs
	D1, D2 := D0+vpTestDayMs, D0+2*vpTestDayMs
	var clockMs atomic.Int64
	clockMs.Store(D0 - 20*60_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }

	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		iv, _ := hlCandleIntervalMs(interval)
		last := clockMs.Load()/iv*iv - iv
		out := make([]hlCandleRaw, 0, 300)
		for i := int64(279); i >= 0; i-- {
			open := last - i*iv
			c := vpCandleClose(coin, open, D1, iv)
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + iv - 1, HasClose: true,
				Open: c, High: c + 0.25, Low: c - 0.25, Close: c, Volume: 1})
		}
		return out, nil
	})

	vpBTC, vpETH := vpTestStrategy("vp-btc", "BTC"), vpTestStrategy("vp-eth", "ETH")
	cvd := tfTestStrategy("cvd-btc", "BTC")
	cfg := &Config{Strategies: []StrategyConfig{vpBTC, vpETH, cvd}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	btcKey, ethKey := volumeProfileKeyFor("BTC"), volumeProfileKeyFor("ETH")
	if req.Observations[btcKey] != feedProfileWindowMs || req.Observations[ethKey] != feedProfileWindowMs || req.Observations[tradeFlowKeyFor("BTC")] == 0 {
		t.Fatalf("observation requirements %+v", req.Observations)
	}
	if _, ok := req.Observations[tradeFlowKeyFor("ETH")]; ok {
		t.Fatal("a profile-only coin must not add a trade-flow requirement")
	}
	if e := req.Strategies["vp-btc"]; !e.VolumeProfile || e.TradeFlow || e.OpenInterest || !e.Timed {
		t.Fatalf("profile strategy requirement %+v", e)
	}
	if e := req.Strategies["cvd-btc"]; e.VolumeProfile || !e.TradeFlow {
		t.Fatalf("the trade-flow strategy requirement changed: %+v", e)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{vpBTC}}); err == nil || !strings.Contains(err.Error(), "cannot supply executed-trade volume profiles") {
		t.Fatalf("market_feed=rest with a profile strategy: %v", err)
	}
	served := dropUncollectableObservations(feedSourceREST, []feedConsumer{{Path: "vp", Loaded: true, Req: req}})
	if len(served[0].Req.Observations) != 0 || served[0].Req.Strategies["vp-btc"].VolumeProfile {
		t.Fatalf("a REST feed source must not claim the profile input: %+v", served[0].Req)
	}
	var many []StrategyConfig
	for i := 0; i <= feedProfileMaxCoins; i++ {
		many = append(many, vpTestStrategy(fmt.Sprintf("vp-%d", i), fmt.Sprintf("C%02d", i)))
	}
	if _, err := deriveFeedRequirements(&Config{Strategies: many}); err == nil || !strings.Contains(err.Error(), "coin budget") {
		t.Fatalf("more profile coins than the seal budget must fail preflight, got %v", err)
	}
	union := unionFeedRequirements([]feedConsumer{{Path: "vp", Loaded: true, Req: req}})
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
		RunID: "profile-harness", Coins: []string{"BTC", "ETH"}, Kinds: []string{feedObservationTradeFlow},
		Segment: 12 * time.Hour, WSURL: fs.url(), StartedAt: clock(),
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
		return strings.Contains(subs, `"coin":"BTC","type":"trades"`) && strings.Contains(subs, `"coin":"ETH","type":"trades"`)
	})
	if n := strings.Count(strings.Join(fs.subscriptions(), " "), `"coin":"BTC","type":"trades"`); n != 1 {
		t.Fatalf("a coin with a profile and a trade-flow consumer must keep one trades subscription, got %d", n)
	}

	profile := func(coin string) *feedProfileState {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.profileStateLocked(coin)
	}
	coverFrom := func(coin string) int64 {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.tradeFlows[tradeFlowKeyFor(coin)].CoverFromMs
	}
	ack := func(coin string, recvMs int64) {
		t.Helper()
		clockMs.Store(recvMs)
		fs.broadcast(t, map[string]any{"channel": "subscriptionResponse", "data": map[string]any{
			"method": "subscribe", "subscription": map[string]any{"type": "trades", "coin": coin}}})
		waitFor(t, coin+" coverage start", func() bool { return coverFrom(coin) != 0 })
	}
	frames := 0
	send := func(recvMs int64, trades ...map[string]any) {
		t.Helper()
		clockMs.Store(recvMs)
		blob, _ := json.Marshal(map[string]any{"channel": "trades", "data": trades})
		owner.handleSocketMessage(blob)
		frames++
		if frames%500 == 0 {
			waitFor(t, "recorder drain", func() bool { return len(rec.queue) == 0 })
		}
	}
	ack("BTC", D0-12*60_000)
	ack("ETH", D0-12*60_000+500)

	var sealer *feedSealer
	sealed := map[int64][]byte{}
	direct := map[int64]*marketSnapshot{}
	type dayCopy struct {
		art                   feedProfileArtifact
		status, code, invCode string
		invalidMs             int64
	}
	copyDay := func(coin string, start int64) dayCopy {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		d := owner.profileStateLocked(coin).Days[start]
		if d == nil {
			return dayCopy{}
		}
		out := dayCopy{status: d.Status, code: d.Code, invCode: d.InvalidCode, invalidMs: d.InvalidMs}
		if d.Artifact != nil {
			out.art = *d.Artifact
		}
		return out
	}
	seal := func(key int64) {
		t.Helper()
		clockMs.Store(key*1000 + 1_000)
		if key%900 != 0 {
			reqs := fullCycleRequirements(sealer.coverage)
			prepareMarketSnapshot(ctx, owner, reqs)
			direct[key] = freezeMarketSnapshot(owner, reqs, feedSealEvaluationID(key), clock(), time.Unix(key, 0))
			return
		}
		sealer.sealOne(ctx, key)
		sealed[key] = append([]byte(nil), sealer.sealBytes(key)...)
		if len(sealed[key]) == 0 {
			t.Fatalf("no seal for key %d: %+v", key, sealer.lookup(key))
		}
		direct[key] = freezeMarketSnapshot(owner, fullCycleRequirements(sealer.coverage), feedSealEvaluationID(key), clock(), time.Unix(key, 0))
	}
	sealer = newFeedSealer(owner, feedSourceWebsocket, "inst-vp", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{900, 300}, time.UnixMilli(D0-vpTestDayMs))
	sealer.startServing(time.UnixMilli(D0 - vpTestDayMs))

	key0, key1, key2, key3 := (D1+5_000)/1000, (D1+2*3_600_000)/1000, (D1+4*3_600_000)/1000, (D2+3_600_000)/1000
	var dupTrade, conflictBase map[string]any
	var btcD0, ethD0 dayCopy
	for recv := D0 - 9*60_000 + 10_000; recv <= D2+3_600_000+30_000; recv += 20_000 {
		var btc []map[string]any
		i := (recv - D0 - 10_000) / 20_000
		switch {
		case recv < D0:
			btc = append(btc, vpTrade("BTC", uint64(2_000_000_000+(recv-D0+10*60_000)/20_000), recv-300, "B", "99", "0.001"))
		case recv < D1:
			side := "B"
			if i%2 == 1 {
				side = "A"
			}
			tr := vpTrade("BTC", uint64(1_000_000_000+i), recv-300, side, vpTestBTCCycle[i%16], "0.001")
			btc = append(btc, tr)
			if recv == D0+3*3_600_000-10_000 {
				dupTrade = tr
			}
			if recv == D0+3*3_600_000+50_000 {
				btc = append(btc, dupTrade)
			}
		default:
			j := (recv - D1 - 10_000) / 20_000
			tr := vpTrade("BTC", uint64(3_000_000_000+j), recv-300, "B", vpTestBTCCycle[(j+3)%16], "0.002")
			btc = append(btc, tr)
			if recv == D1+3*3_600_000+10_000 {
				btc = append(btc, vpTrade("BTC", 1_999_999_999, D1-30_000, "A", "107", "5"))
			}
			if recv == D1+5*3_600_000-10_000 {
				conflictBase = tr
			}
			if recv == D1+5*3_600_000+30_000 {
				cp := map[string]any{}
				for k, v := range conflictBase {
					cp[k] = v
				}
				cp["sz"] = "9.5"
				btc = append(btc, cp)
			}
		}
		send(recv, btc...)
		ethRecv := recv + 5_000
		if !(ethRecv >= D0+5*3_600_000 && ethRecv < D0+5*3_600_000+40_000) {
			ethPx := "2000.5"
			if (recv/20_000)%3 == 0 {
				ethPx = "2001"
			}
			send(ethRecv, vpTrade("ETH", uint64(5_000_000_000+(recv-D0+10*60_000)/20_000), ethRecv-300, "B", ethPx, "0.01"))
		}
		switch {
		case recv+20_000 > key0*1000 && recv <= key0*1000:
			seal(key0)
		case recv+20_000 > key1*1000 && recv <= key1*1000:
			seal(key1)
		case recv+20_000 > key2*1000 && recv <= key2*1000:
			seal(key2)
			btcD0, ethD0 = copyDay("BTC", D0), copyDay("ETH", D0)
		case recv+20_000 > key3*1000 && recv <= key3*1000:
			seal(key3)
		}
	}
	waitFor(t, "recorder drain", func() bool { return len(rec.queue) == 0 })

	btcProf := profile("BTC")
	art := btcD0.art
	btcD1 := copyDay("BTC", D1)
	d1Status, d1Code := btcD1.status, btcD1.code
	d0Invalid, d0Code := btcD0.invalidMs, btcD0.invCode
	ethCode := ethD0.code
	wantPairs := []feedProfilePair{{"100", 54_000_000}, {"101", 27_000_000}, {"102", 108_000_000}, {"103", 27_000_000},
		{"104", 54_000_000}, {"105", 81_000_000}, {"106", 27_000_000}, {"108", 54_000_000}}
	if !reflect.DeepEqual(art.Pairs, wantPairs) || art.Trades != 4320 || art.Units != 432_000_000 || art.CompleteMinutes != feedProfileMinutes {
		t.Fatalf("BTC session D0 artifact %+v, want the hand-calculated totals %v over 4320 trades", art, wantPairs)
	}
	if art.AvailableMs != D1+10_000 || art.FirstEventMs != D0+9_700 || art.LastEventMs != D1-10_300 {
		t.Fatalf("BTC session D0 availability %d and event bounds [%d, %d]", art.AvailableMs, art.FirstEventMs, art.LastEventMs)
	}
	if d0Invalid != D1+3*3_600_000+10_000 || d0Code != feedProfileCodeLate {
		t.Fatalf("the late session-D0 trade must revoke the frozen artifact for later decisions: %d %q", d0Invalid, d0Code)
	}
	if d1Status != feedProfileDayUnavailable || d1Code != feedProfileCodeConflict {
		t.Fatalf("a conflicting duplicate must make session D1 unavailable: %s %s", d1Status, d1Code)
	}
	if ethCode != feedProfileCodeMinute {
		t.Fatalf("an ETH frame silence must leave ETH session D0 incomplete, got %q", ethCode)
	}
	owner.feedMu.Lock()
	_, kept := btcProf.Days[D0]
	owner.feedMu.Unlock()
	if kept {
		t.Fatal("the session two days back must be trimmed from the owner")
	}

	wantStatus := map[int64]map[string]string{
		key0: {"BTC": feedProfileStatusPending, "ETH": feedProfileStatusUnavailable},
		key1: {"BTC": feedProfileStatusAvailable, "ETH": feedProfileStatusUnavailable},
		key2: {"BTC": feedProfileStatusInvalidated, "ETH": feedProfileStatusUnavailable},
		key3: {"BTC": feedProfileStatusUnavailable, "ETH": feedProfileStatusAvailable},
	}
	for key, want := range wantStatus {
		if key%900 != 0 {
			for coin, st := range want {
				sessions := direct[key].profiles[volumeProfileKeyFor(coin)].Sessions
				if len(sessions) != 2 {
					t.Fatalf("a cutoff inside the first hour of a session must seal both prior sessions, got %d", len(sessions))
				}
				if got := sessions[1].Status; got != st {
					t.Fatalf("snapshot %d %s profile status %s, want %s", key, coin, got, st)
				}
			}
			continue
		}
		doc, err := decodeFeedSeal(sealed[key], key)
		if err != nil {
			t.Fatalf("decode seal %d: %v", key, err)
		}
		if doc.V != feedSealVersionProfile || len(doc.VolumeProfiles) != 2 || len(doc.TradeFlows) != 1 {
			t.Fatalf("seal %d v%d carries %d profiles and %d trade flows", key, doc.V, len(doc.VolumeProfiles), len(doc.TradeFlows))
		}
		for _, p := range doc.VolumeProfiles {
			last := p.Sessions[len(p.Sessions)-1]
			if len(p.Sessions) != 1 || last.Status != want[p.Coin] {
				t.Fatalf("seal %d %s profile sessions %d, status %s (%s: %s), want one %s", key, p.Coin, len(p.Sessions), last.Status, last.Code, last.Reason, want[p.Coin])
			}
			if last.Artifact != nil && last.Artifact.AvailableMs > key*1000 {
				t.Fatalf("seal %d carries an artifact that became available after its cutoff", key)
			}
		}
		if len(sealed[key]) > estimate {
			t.Fatalf("seal %d is %d bytes over the %d-byte estimate", key, len(sealed[key]), estimate)
		}
	}
	for key, blob := range sealed {
		sealer.sealOne(ctx, key)
		if !bytes.Equal(blob, sealer.sealBytes(key)) {
			t.Fatalf("later trades, revocations or a second attempt changed immutable seal %d", key)
		}
	}

	for name, mutate := range map[string]func(*feedSealDoc){
		"version":  func(d *feedSealDoc) { d.V = feedSealVersionMacro },
		"contract": func(d *feedSealDoc) { d.VolumeProfiles[0].MaxEntries++ },
		"kind":     func(d *feedSealDoc) { d.VolumeProfiles[0].Kind = feedObservationOpenInterest },
	} {
		cp, _ := decodeFeedSeal(sealed[key1], key1)
		mutate(cp)
		if blob, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(blob, key1); err == nil {
				t.Fatalf("a seal with a %s mismatch decoded", name)
			}
		}
	}
	for name, mutate := range map[string]func(*marketVolumeProfileArtifact){
		"hash":   func(a *marketVolumeProfileArtifact) { a.SHA256 = strings.Repeat("0", 64) },
		"size":   func(a *marketVolumeProfileArtifact) { a.Prices[0][1] = "0.55" },
		"order":  func(a *marketVolumeProfileArtifact) { a.Prices[0], a.Prices[1] = a.Prices[1], a.Prices[0] },
		"window": func(a *marketVolumeProfileArtifact) { a.AvailableMs = key1*1000 + 1 },
	} {
		cp, _ := decodeFeedSeal(sealed[key1], key1)
		for i := range cp.VolumeProfiles {
			if cp.VolumeProfiles[i].Coin == "BTC" {
				mutate(cp.VolumeProfiles[i].Sessions[0].Artifact)
			}
		}
		blob, _, err := encodeFeedSeal(cp)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeFeedSeal(blob, key1)
		if err != nil {
			t.Fatalf("a malformed %s profile must not reject the whole seal: %v", name, err)
		}
		snap := got.snapshot()
		entry := snap.profiles[btcKey]
		if len(entry.Sessions) != 1 || entry.Sessions[0].Status != feedProfileStatusUnavailable || entry.Sessions[0].Code != feedProfileCodeSealMalformed || entry.Sessions[0].Artifact != nil {
			t.Fatalf("a malformed %s profile must normalize to unavailable metadata: %+v", name, entry.Sessions)
		}
		if !snap.keys[feedKeyFor("BTC", "15m")].Readiness.Ready {
			t.Fatalf("a malformed %s profile must leave the candle frame ready", name)
		}
	}

	sockDir, err := os.MkdirTemp("", "vp")
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
	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, 72*time.Hour
	descs := client.Describe(ctx)
	if descs[0].Err != nil || descs[0].Header.SealVersion != feedSealVersionProfile || descs[0].Header.PayloadVersion != marketSnapshotVersion {
		t.Fatalf("describe: %+v", descs[0])
	}
	if _, problems := sharedFeedCompatibilityLines("test", descs, req, []int{900, 300}); len(problems) != 0 {
		t.Fatalf("a feed serving the profile reported coverage problems: %v", problems)
	}
	reqs := cycleRequirementsForDue([]StrategyConfig{vpBTC, vpETH, cvd}, req)
	payloads := map[string]*marketPayload{}
	for _, sc := range []StrategyConfig{vpBTC, vpETH} {
		local := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: direct[key0]}
		payloads[fmt.Sprintf("payload_%s_%d.json", strings.ToLower(sc.ID[3:]), key0)] = oiTestPayload(t, local, sc)
	}
	for _, key := range []int64{key1, key2, key3} {
		snap, rep := client.Fetch(ctx, key, reqs)
		if rep.Status != feedFetchSealed || len(rep.Gaps) != 0 {
			t.Fatalf("fetch key %d: %+v", key, rep)
		}
		shared := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, SharedKey: key}
		local := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: direct[key]}
		for _, sc := range []StrategyConfig{vpBTC, vpETH} {
			if hold := shared.holdFor(sc); hold.Held {
				t.Fatalf("%s at %d was held for a profile input: %+v", sc.ID, key, hold)
			}
			sp, lp := oiTestPayload(t, shared, sc), oiTestPayload(t, local, sc)
			if len(sp.TradeFlows) != 0 || len(sp.Observations) != 0 || len(sp.VolumeProfiles) != 1 {
				t.Fatalf("%s payload carries %d trade flows, %d observations, %d profiles", sc.ID, len(sp.TradeFlows), len(sp.Observations), len(sp.VolumeProfiles))
			}
			sj, _ := json.Marshal(sp.VolumeProfiles)
			lj, _ := json.Marshal(lp.VolumeProfiles)
			if !bytes.Equal(sj, lj) {
				t.Fatalf("%s at %d: the shared-seal profile differs from the websocket-mode profile:\n%s\n%s", sc.ID, key, sj, lj)
			}
			if sp.Version != marketSnapshotVersion {
				t.Fatalf("payload version %d", sp.Version)
			}
			if blob, _ := json.Marshal(sp); len(blob) > marketPayloadMaxBytes {
				t.Fatalf("payload %d bytes over the cap", len(blob))
			}
			payloads[fmt.Sprintf("payload_%s_%d.json", strings.ToLower(sc.ID[3:]), key)] = sp
		}
		cp := oiTestPayload(t, shared, cvd)
		if len(cp.VolumeProfiles) != 0 || len(cp.TradeFlows) != 1 || !cp.TradeFlows["BTC|trade_flow"].Available {
			t.Fatalf("the trade-flow consumer payload changed: profiles %d, flows %+v", len(cp.VolumeProfiles), cp.TradeFlows)
		}
		batch, err := shared.batchPayload(hlBatchKey{Symbol: "BTC", Timeframe: "15m", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit}, []StrategyConfig{vpBTC})
		if err != nil {
			t.Fatalf("batch payload at %d: %v", key, err)
		}
		if bj, _ := json.Marshal(batch.VolumeProfiles); !bytes.Contains(bj, []byte(`"BTC|volume_profile"`)) {
			t.Fatalf("the batch payload must carry the BTC profile: %s", bj)
		}
	}

	onlyCVD, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{cvd}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := freezeMarketSnapshot(owner, fullCycleRequirements(onlyCVD), "legacy", clock(), time.Unix(key3, 0))
	ldoc, err := feedSealDocFromSnapshot(legacy, key3, feedSourceWebsocket, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	lblob, _, _ := encodeFeedSeal(ldoc)
	if ldoc.V != feedSealVersionTrades || bytes.Contains(lblob, []byte("volume_profiles")) {
		t.Fatalf("a seal without a profile need must keep version %d and its fields, got v%d", feedSealVersionTrades, ldoc.V)
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
	nextReq := onlyCVD
	nextReq.AccountingCoins = nil
	select {
	case <-owner.ApplyGeneration(context.Background(), nextReq):
	case <-time.After(5 * time.Second):
		t.Fatal("the reduced generation did not publish")
	}
	owner.feedMu.Lock()
	btcState, ethState := owner.tradeFlows[tradeFlowKeyFor("BTC")], owner.tradeFlows[tradeFlowKeyFor("ETH")]
	owner.feedMu.Unlock()
	if btcState == nil || btcState.profile != nil || btcState.internal || ethState != nil {
		t.Fatalf("removing the profile consumers must drop the profile state and the profile-only coin and keep the trade flow: btc=%+v eth=%v", btcState, ethState)
	}
	for key, blob := range sealed {
		if !bytes.Equal(blob, sealer.sealBytes(key)) {
			t.Fatalf("a generation change mutated seal %d", key)
		}
	}

	if out := os.Getenv("GO_TRADER_PROFILE_HARNESS_OUT"); out != "" {
		vpWriteHarness(t, out, recDir, payloads, map[string]any{
			"key0": key0, "key1": key1, "key2": key2, "key3": key3, "d0": D0, "d1": D1,
		})
	}
}

func vpWriteHarness(t *testing.T, out, recDir string, payloads map[string]*marketPayload, keys map[string]any) {
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
	var cases []map[string]any
	for name, p := range payloads {
		blob, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), blob, 0o644); err != nil {
			t.Fatal(err)
		}
		coin := "BTC"
		if strings.Contains(name, "_eth_") {
			coin = "ETH"
		}
		cases = append(cases, map[string]any{"payload": name, "coin": coin})
	}
	harness := map[string]any{"timeframe": "15m", "keys": keys, "cases": cases,
		"params": map[string]any{"profile_bins": 4, "value_area_fraction": 0.6},
		"hand": map[string]any{"val": "102", "vah": "106", "poc_low": "102", "poc_high": "104", "wide_val": "100", "wide_vah": "106",
			"wide_fraction": 0.8, "bounds_val": "100", "bounds_vah": "108", "total_volume": 4.32}}
	blob, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "harness.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func vpFeedOwner(t *testing.T, clockMs *atomic.Int64, coins ...string) *marketFeedOwner {
	t.Helper()
	owner := newMarketFeedOwner(func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }, t.Logf)
	needs := map[feedObservationKey]int64{}
	for _, c := range coins {
		needs[volumeProfileKeyFor(c)] = feedProfileWindowMs
	}
	owner.feedMu.Lock()
	owner.applyObservationNeedsLocked(needs)
	owner.feedMu.Unlock()
	owner.SetConnected(true)
	return owner
}

func vpIngest(owner *marketFeedOwner, clockMs *atomic.Int64, recvMs int64, trades ...map[string]any) {
	clockMs.Store(recvMs)
	data, _ := json.Marshal(trades)
	owner.IngestTradesFrame(data, time.UnixMilli(recvMs), data)
}

func vpSession(owner *marketFeedOwner, coin string, cutoffMs int64) feedProfileSessionSnapshot {
	owner.feedMu.Lock()
	defer owner.feedMu.Unlock()
	snap := freezeVolumeProfile(owner.profileStateLocked(coin), volumeProfileKeyFor(coin), feedProfileWindowMs, cutoffMs, cutoffMs)
	return snap.Sessions[len(snap.Sessions)-1]
}

func TestVolumeProfileRestartWarmupAndLimits(t *testing.T) {
	D0 := int64(1_790_000_000_000) / vpTestDayMs * vpTestDayMs
	D1, D2 := D0+vpTestDayMs, D0+2*vpTestDayMs
	var clockMs atomic.Int64
	clockMs.Store(D0 + 6*3_600_000)
	owner := vpFeedOwner(t, &clockMs, "BTC")
	owner.IngestTradeSubscription("BTC", time.UnixMilli(D0+6*3_600_000), []byte("{}"))
	tid := uint64(1)
	for recv := D0 + 6*3_600_000 + 10_000; recv <= D2+3_600_000; recv += 20_000 {
		px := "100"
		if tid%2 == 0 {
			px = "101"
		}
		vpIngest(owner, &clockMs, recv, vpTrade("BTC", tid, recv-300, "B", px, "0.01"))
		tid++
		if recv == D1+3_600_000-10_000 {
			if s := vpSession(owner, "BTC", D1+3_600_000); s.Status != feedProfileStatusPending || s.Code != feedProfileCodeNotFinal {
				t.Fatalf("a session that began before this owner started must hold as pending, got %+v", s)
			}
		}
	}
	if s := vpSession(owner, "BTC", D2+3_600_000); s.Status != feedProfileStatusAvailable || s.Artifact == nil || s.Artifact.Trades != 4320 {
		t.Fatalf("the first complete session after a restart must become available, got %+v", s)
	}
	frozen := vpSession(owner, "BTC", D2+3_600_000).Artifact
	lateRecv := D2 + 3_600_000 + 20_000
	vpIngest(owner, &clockMs, lateRecv, vpTrade("BTC", 99_000_001, D1+5*3_600_000+1_000, "A", "100", "7"), vpTrade("BTC", tid, lateRecv-300, "B", "101", "0.01"))
	owner.feedMu.Lock()
	evicted := owner.tradeFlows[tradeFlowKeyFor("BTC")].interval(D1+5*3_600_000) == nil
	owner.feedMu.Unlock()
	if !evicted {
		t.Fatal("the profile-only trade state must have evicted the minute 20 hours back")
	}
	if s := vpSession(owner, "BTC", D2+3_600_000+3_600_000); s.Status != feedProfileStatusInvalidated || s.Code != feedProfileCodeLate || s.InvalidMs != lateRecv {
		t.Fatalf("a new trade for a finalized minute whose interval and identity were evicted must still revoke the session: %+v", s)
	}
	if s := vpSession(owner, "BTC", D2+3_600_000); s.Status != feedProfileStatusAvailable || s.Artifact != frozen || s.Artifact.Units != frozen.Units {
		t.Fatalf("a revocation after the cutoff must leave the earlier view and the frozen numbers unchanged: %+v", s)
	}

	for name, tc := range map[string]struct {
		trade func(i int) map[string]any
		n     int
		code  string
	}{
		"entries": {func(i int) map[string]any {
			return vpTrade("ETH", uint64(10_000+i), D0+10_000, "B", fmt.Sprintf("%d.5", 1000+i), "0.001")
		}, feedProfileMaxEntries + 1, feedProfileCodeEntries},
		"field": {func(i int) map[string]any {
			return vpTrade("ETH", uint64(20_000+i), D0+10_000, "B", "1000000000000000.12345678", "0.001")
		}, 1, feedProfileCodeFieldLength},
		"overflow": {func(i int) map[string]any {
			return vpTrade("ETH", uint64(30_000+i), D0+10_000, "B", "1000", "9999999999")
		}, 1000, feedProfileCodeOverflow},
	} {
		var ck atomic.Int64
		ck.Store(D0 - 120_000)
		o := vpFeedOwner(t, &ck, "ETH")
		o.IngestTradeSubscription("ETH", time.UnixMilli(D0-120_000), []byte("{}"))
		for recv := D0 - 100_000; recv <= D0; recv += 20_000 {
			vpIngest(o, &ck, recv, vpTrade("ETH", uint64(recv/1000), recv-300, "B", "1000", "0.001"))
		}
		var batch []map[string]any
		for i := 0; i < tc.n; i++ {
			batch = append(batch, tc.trade(i))
		}
		vpIngest(o, &ck, D0+10_500, batch...)
		o.feedMu.Lock()
		d := o.profileStateLocked("ETH").Days[D0]
		status, code, prices := d.Status, d.Code, d.prices
		o.feedMu.Unlock()
		if status != feedProfileDayUnavailable || code != tc.code || prices != nil {
			t.Fatalf("%s limit: day status %s code %s, map released %v", name, status, code, prices == nil)
		}
	}

	var ck atomic.Int64
	ck.Store(D0 - 120_000)
	flat := vpFeedOwner(t, &ck, "SOL")
	flat.IngestTradeSubscription("SOL", time.UnixMilli(D0-120_000), []byte("{}"))
	for recv := D0 - 100_000; recv <= D1+30_000; recv += 20_000 {
		vpIngest(flat, &ck, recv, vpTrade("SOL", uint64(recv/1000), recv-300, "B", "50", "1"))
	}
	if s := vpSession(flat, "SOL", D1+3_600_000); s.Status != feedProfileStatusUnavailable || s.Code != feedProfileCodeZeroRange {
		t.Fatalf("a complete session at one price must be unavailable, got %+v", s)
	}
}

func vpMaxArtifact(coin string, start int64) *feedProfileArtifact {
	art := &feedProfileArtifact{Coin: coin, SessionStartMs: start, SessionEndMs: start + feedProfileSessionMs, Trades: 1 << 40,
		TradesHash: strings.Repeat("f", 64), FirstEventMs: start, LastEventMs: start + feedProfileSessionMs - 1,
		CompleteMinutes: feedProfileMinutes, AvailableMs: start + feedProfileSessionMs + feedTradeReorderGraceMs}
	for i := 0; i < feedProfileMaxEntries; i++ {
		art.Pairs = append(art.Pairs, feedProfilePair{Px: fmt.Sprintf("%d.1234567", 1_000_000_000_000_000+i), Units: 562_949_953_421_311})
		art.Units += 562_949_953_421_311
	}
	art.SHA256 = feedProfileHash(feedProfileCanonicalText(coin, art.SessionStartMs, art.SessionEndMs, art.Trades, formatFeedUnits(art.Units),
		art.TradesHash, art.FirstEventMs, art.LastEventMs, art.prices()))
	return art
}

func TestVolumeProfileCapacityAtLimitsAndEntryOnlyIsolation(t *testing.T) {
	cutoff := int64(1_800_000_000_000)/vpTestDayMs*vpTestDayMs + 30*60_000
	starts := feedProfileSealSessions(cutoff)
	if len(starts) != 2 {
		t.Fatalf("a cutoff 30 minutes into a session must seal two sessions, got %v", starts)
	}
	var strategies []StrategyConfig
	snap := &marketSnapshot{Version: marketSnapshotVersion, EvaluationID: feedSealEvaluationID(cutoff / 1000), SealedAt: time.UnixMilli(cutoff),
		Deadline: time.UnixMilli(cutoff), keys: map[marketFeedKey]*marketSnapshotKey{}, mids: map[string]feedMid{}, funding: map[string]feedFunding{},
		observations: map[feedObservationKey]*marketSnapshotObservation{}, tradeFlows: map[feedObservationKey]*marketSnapshotTradeFlow{},
		profiles: map[feedObservationKey]*marketSnapshotProfile{}}
	for i := 0; i < feedProfileMaxCoins; i++ {
		coin := fmt.Sprintf("C%02d", i)
		sc := vpTestStrategy("vp-"+coin, coin)
		strategies = append(strategies, sc)
		key := volumeProfileKeyFor(coin)
		entry := &marketSnapshotProfile{Key: key, WindowMs: feedProfileWindowMs, CutoffMs: cutoff,
			Readiness: feedProfileReadiness{Key: key, Status: feedProfileStatusAvailable, Ready: true, SessionStartMs: starts[1]}}
		for _, start := range starts {
			entry.Sessions = append(entry.Sessions, feedProfileSessionSnapshot{SessionStartMs: start, Status: feedProfileStatusAvailable, Artifact: vpMaxArtifact(coin, start)})
		}
		snap.profiles[key] = entry
		fk := feedKeyFor(coin, "15m")
		bars := make([]feedBar, 0, 400)
		for b := int64(400); b > 0; b-- {
			open := cutoff/vpTestBarMs*vpTestBarMs - b*vpTestBarMs
			bars = append(bars, feedBar{OpenMs: open, CloseMs: open + vpTestBarMs - 1, HasClose: true, Open: 123456.789, High: 123460.123, Low: 123450.456, Close: 123455.789, Volume: 98765.4321})
		}
		snap.keys[fk] = &marketSnapshotKey{Key: fk, IntervalMs: vpTestBarMs, Bars: bars,
			Readiness: feedKeyReadiness{Key: fk, Bars: len(bars), Required: len(bars), Ready: true, Status: feedStatusReady, Source: "ws",
				FirstOpenMs: bars[0].OpenMs, LastOpenMs: bars[len(bars)-1].OpenMs, LastCloseMs: bars[len(bars)-1].CloseMs}}
	}
	req, err := deriveFeedRequirements(&Config{Strategies: strategies})
	if err != nil {
		t.Fatalf("the maximum admitted profile coin count must derive: %v", err)
	}
	req.AccountingCoins = nil
	union := unionFeedRequirements([]feedConsumer{{Path: "a", Loaded: true, Req: req}, {Path: "b", Loaded: true, Req: req}})
	if err := volumeProfileUnionError(union); err != nil {
		t.Fatalf("two consumers of the same coins must not multiply the profile budget: %v", err)
	}
	extra := vpTestStrategy("vp-extra", "EXTRA")
	reqExtra, _ := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{extra}})
	if err := volumeProfileUnionError(unionFeedRequirements([]feedConsumer{{Path: "a", Loaded: true, Req: req}, {Path: "c", Loaded: true, Req: reqExtra}})); err == nil {
		t.Fatal("a consumer union above the profile coin budget must be refused")
	}
	doc, err := feedSealDocFromSnapshot(snap, cutoff/1000, feedSourceWebsocket, "cap")
	if err != nil {
		t.Fatal(err)
	}
	blob, _, err := encodeFeedSeal(doc)
	if err != nil {
		t.Fatalf("a maximum %d-coin two-session profile seal must encode under the cap: %v", feedProfileMaxCoins, err)
	}
	est := estimateFeedSealBytes(union)
	if len(blob) > est || len(blob) > feedSealMaxBytes || est > feedSealMaxBytes {
		t.Fatalf("seal %d bytes, estimate %d, cap %d", len(blob), est, feedSealMaxBytes)
	}
	decoded, err := decodeFeedSeal(blob, cutoff/1000)
	if err != nil {
		t.Fatalf("decode the maximum seal: %v", err)
	}
	shared := decoded.snapshot()
	for _, s := range shared.profiles[volumeProfileKeyFor("C00")].Sessions {
		if s.Status != feedProfileStatusAvailable || s.Artifact == nil || len(s.Artifact.Pairs) != feedProfileMaxEntries {
			t.Fatalf("a maximum artifact must survive the seal round trip: %s %s", s.Status, s.Reason)
		}
	}
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: shared, SharedKey: cutoff / 1000}
	single := oiTestPayload(t, feed, strategies[0])
	sj, _ := json.Marshal(marketStdinEnvelope{Version: hlBatchProtocolVersionMarket, Market: single})
	if len(sj) > marketPayloadMaxBytes || len(single.VolumeProfiles["C00|volume_profile"].Sessions) != 2 {
		t.Fatalf("a maximum single payload is %d bytes (cap %d)", len(sj), marketPayloadMaxBytes)
	}
	batch, err := feed.batchPayload(hlBatchKey{Symbol: "C00", Timeframe: "15m", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit}, []StrategyConfig{strategies[0]})
	if err != nil {
		t.Fatal(err)
	}
	bj, _ := json.Marshal(batch)
	if len(bj) > marketPayloadMaxBytes {
		t.Fatalf("a maximum batch payload is %d bytes (cap %d)", len(bj), marketPayloadMaxBytes)
	}
	t.Logf("capacity at the limits: %d-coin two-session seal %d bytes (estimate %d, cap %d); single payload %d bytes, batch payload %d bytes (cap %d)",
		feedProfileMaxCoins, len(blob), est, feedSealMaxBytes, len(sj), len(bj), marketPayloadMaxBytes)

	for name, mutate := range map[string]func(*marketSnapshot){
		"missing":   func(s *marketSnapshot) { delete(s.profiles, volumeProfileKeyFor("C00")) },
		"malformed": func(s *marketSnapshot) { s.profiles[volumeProfileKeyFor("C00")].Sessions[1].Artifact.Pairs[0].Units++ },
	} {
		cp, _ := decodeFeedSeal(blob, cutoff/1000)
		degraded := cp.snapshot()
		mutate(degraded)
		dd, _ := feedSealDocFromSnapshot(degraded, cutoff/1000, feedSourceWebsocket, "cap")
		db, _, _ := encodeFeedSeal(dd)
		got, err := decodeFeedSeal(db, cutoff/1000)
		if err != nil {
			t.Fatalf("%s profile: the seal must still decode: %v", name, err)
		}
		gs := got.snapshot()
		ctx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: gs, SharedKey: cutoff / 1000}
		if hold := ctx.holdFor(strategies[0]); hold.Held {
			t.Fatalf("%s profile must not hold the whole evaluation: %+v", name, hold)
		}
		p := oiTestPayload(t, ctx, strategies[0])
		if _, ok := p.Frames["C00|15m"]; !ok {
			t.Fatalf("%s profile must leave the candle frame in the payload", name)
		}
		if _, err := ctx.batchPayload(hlBatchKey{Symbol: "C00", Timeframe: "15m", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit}, []StrategyConfig{strategies[0]}); err != nil {
			t.Fatalf("%s profile must not fail the batch payload: %v", name, err)
		}
		vp := p.VolumeProfiles["C00|volume_profile"]
		switch name {
		case "missing":
			if vp.Available || !strings.Contains(vp.Reason, "not in the sealed snapshot") {
				t.Fatalf("a missing profile must be an unavailable entry with a reason: %+v", vp.Reason)
			}
			if gaps := applySealCoverage(gs, fullCycleRequirements(req)); len(gaps) != 1 || !strings.Contains(gaps[0], "C00|volume_profile is not in the seal") {
				t.Fatalf("seal coverage gaps %v", gaps)
			}
		case "malformed":
			if vp.Sessions[1].Status != feedProfileStatusUnavailable || vp.Sessions[1].ReasonCode != feedProfileCodeSealMalformed || vp.Sessions[1].Artifact != nil {
				t.Fatalf("a malformed profile must reach the worker only as unavailable metadata: %+v", vp.Sessions[1])
			}
			if other := gs.profiles[volumeProfileKeyFor("C01")].Sessions[1]; other.Status != feedProfileStatusAvailable {
				t.Fatalf("one coin's malformed profile must not affect another coin: %s", other.Status)
			}
		}
	}
}

func TestVolumeProfileAttachedToAnActiveTradeStreamWaitsForAFullSession(t *testing.T) {
	D0 := int64(1_790_000_000_000) / vpTestDayMs * vpTestDayMs
	D1, D2 := D0+vpTestDayMs, D0+2*vpTestDayMs
	var clockMs atomic.Int64
	clockMs.Store(D0 - 180_000)
	owner := newMarketFeedOwner(func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }, t.Logf)
	owner.feedMu.Lock()
	owner.applyObservationNeedsLocked(map[feedObservationKey]int64{tradeFlowKeyFor("BTC"): 2 * 3_600_000})
	owner.feedMu.Unlock()
	owner.SetConnected(true)
	owner.IngestTradeSubscription("BTC", time.UnixMilli(D0-180_000), []byte("{}"))
	tid := uint64(1)
	attached := false
	for recv := D0 - 170_000; recv <= D2+30_000; recv += 20_000 {
		px := "100"
		if tid%2 == 0 {
			px = "101"
		}
		vpIngest(owner, &clockMs, recv, vpTrade("BTC", tid, recv-300, "B", px, "0.01"))
		tid++
		if !attached && recv >= D0+30_000 {
			owner.feedMu.Lock()
			owner.applyObservationNeedsLocked(map[feedObservationKey]int64{tradeFlowKeyFor("BTC"): 2 * 3_600_000, volumeProfileKeyFor("BTC"): feedProfileWindowMs})
			owner.feedMu.Unlock()
			attached = true
		}
	}
	if s := vpSession(owner, "BTC", D1+2*3_600_000); s.Status != feedProfileStatusUnavailable || s.Code != feedProfileCodeAttached {
		t.Fatalf("a profile attached after trades of its first minute were accepted must not seal that session: %+v", s)
	}
	if s := vpSession(owner, "BTC", D2+3_600_000); s.Status != feedProfileStatusAvailable || s.Artifact == nil || s.Artifact.Trades != 4320 {
		t.Fatalf("the next full session after the attach must become available: %+v", s)
	}
}
