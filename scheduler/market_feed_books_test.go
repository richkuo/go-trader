package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	l2TestBarMs      = int64(60_000)
	l2TestStrategy   = "l2_book_fixture"
	l2TestTickPeriod = time.Hour
)

func l2RegisterFixture(t *testing.T) {
	t.Helper()
	feedBookStrategyNeeds[l2TestStrategy] = func(sc StrategyConfig, intervalMs int64) (feedBookNeed, error) {
		p := sc.OpenStrategy.Params
		w, err := openInterestParamInt(p, "window_ms", 60_000)
		if err != nil {
			return feedBookNeed{}, err
		}
		c, err := openInterestParamInt(p, "capacity", 256)
		if err != nil {
			return feedBookNeed{}, err
		}
		d, err := openInterestParamInt(p, "depth", 5)
		if err != nil {
			return feedBookNeed{}, err
		}
		need := contractBookNeed(w, int(c), int(d))
		if v, ok := p["contract"].(string); ok {
			need.Contract = v
		}
		return need, nil
	}
	t.Cleanup(func() {
		delete(feedBookStrategyNeeds, l2TestStrategy)
	})
}

func l2TestStrategyConfig(id, coin string, params map[string]interface{}) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"donchian_breakout", coin, "1m", "--mode=paper"}, OpenStrategy: StrategyRef{Name: l2TestStrategy, Params: params}}
}

func l2TestLevels(base, step int64, down bool, scale, tag int) []map[string]any {
	out := make([]map[string]any, 0, feedBookMaxLevels)
	for i := int64(0); i < feedBookMaxLevels; i++ {
		px := base + i*step
		if down {
			px = base - i*step
		}
		out = append(out, map[string]any{"px": fmt.Sprintf("%d.0", px), "sz": fmt.Sprintf("%d.%d1", scale, tag%10), "n": 1 + (tag+int(i))%7})
	}
	return out
}

func l2TestBook(coin string, eventMs int64, tag int) map[string]any {
	base := int64(100_000)
	if coin == "ETH" {
		base = 2_500
	}
	return map[string]any{"channel": "l2Book", "data": map[string]any{
		"coin": coin, "time": eventMs, "levels": []any{l2TestLevels(base, 1, true, 3, tag), l2TestLevels(base+1, 1, false, 4, tag)},
	}}
}

func l2TestAck(coin string, extra map[string]any) map[string]any {
	sub := map[string]any{"type": "l2Book", "coin": coin, "nSigFigs": nil, "mantissa": nil, "fast": false}
	for k, v := range extra {
		sub[k] = v
	}
	return map[string]any{"channel": "subscriptionResponse", "data": map[string]any{"method": "subscribe", "subscription": sub}}
}

func l2StubCandles(t *testing.T, clockMs *atomic.Int64) {
	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		last := clockMs.Load()/l2TestBarMs*l2TestBarMs - l2TestBarMs
		out := make([]hlCandleRaw, 0, 300)
		for i := int64(279); i >= 0; i-- {
			open := last - i*l2TestBarMs
			c := 100_000 + 50*math.Sin(float64(open/l2TestBarMs)*0.7)
			if coin == "ETH" {
				c = 2_500 + 5*math.Sin(float64(open/l2TestBarMs)*0.9)
			}
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + l2TestBarMs - 1, HasClose: true, Open: c - 1, High: c + 3, Low: c - 3, Close: c, Volume: 10})
		}
		return out, nil
	})
}

func l2DecodeEnvelope(t *testing.T, blob []byte) *marketPayload {
	t.Helper()
	var env struct {
		V      int            `json:"v"`
		Market *marketPayload `json:"market"`
	}
	if err := json.Unmarshal(blob, &env); err != nil || env.Market == nil {
		t.Fatalf("decode stdin envelope: %v", err)
	}
	return env.Market
}

func (fs *feedSocketServer) broadcastRaw(t *testing.T, payload []byte) {
	t.Helper()
	fs.mu.Lock()
	conns := append([]*websocket.Conn(nil), fs.conns...)
	fs.mu.Unlock()
	for _, c := range conns {
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			t.Fatalf("broadcast raw: %v", err)
		}
	}
}

func (fs *feedSocketServer) unsubscriptions() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.unsubs...)
}

func l2TestPeer(t *testing.T, dir string, sealVersion, payloadVersion int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("peer-s%d-p%d.sock", sealVersion, payloadVersion))
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
			_ = writeFeedJSONFrame(conn, map[string]any{"v": feedWireVersion, "status": feedWireStatusDescribe, "instance": "peer",
				"source": feedSourceWebsocket, "generation": 1, "seal_version": sealVersion, "payload_version": payloadVersion})
			conn.Close()
		}
	}()
	return path
}

func TestL2BookRequirementsSurviveEveryUnionAndRefuseConflicts(t *testing.T) {
	l2RegisterFixture(t)
	btc := l2TestStrategyConfig("l2-btc", "BTC", map[string]interface{}{"window_ms": float64(60_000), "capacity": float64(256), "depth": float64(5)})
	btcWide := l2TestStrategyConfig("l2-btc-wide", "BTC", map[string]interface{}{"window_ms": float64(120_000), "capacity": float64(64), "depth": float64(10)})
	eth := l2TestStrategyConfig("l2-eth", "ETH", nil)
	req, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{btc, btcWide, eth}})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	want := contractBookNeed(120_000, 256, 10)
	if got := req.Books[l2BookKeyFor("BTC")]; got != want {
		t.Fatalf("BTC need %+v, want the per-dimension maxima %+v", got, want)
	}
	if e := req.Strategies["l2-btc"]; e.Book == nil || *e.Book != contractBookNeed(60_000, 256, 5) {
		t.Fatalf("strategy requirement keeps its own need: %+v", e.Book)
	}
	if len(req.Observations) != 0 {
		t.Fatalf("book needs must not enter the scalar observation map: %+v", req.Observations)
	}
	other := l2TestStrategyConfig("l2-other", "BTC", map[string]interface{}{"contract": "other-contract/v9"})
	if _, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{btc, other}}); err == nil || !strings.Contains(err.Error(), "l2_book") {
		t.Fatalf("a need with another contract must be refused before application, got %v", err)
	}
	for name, params := range map[string]map[string]interface{}{
		"window":   {"window_ms": float64(feedBookMaxWindowMs + 1)},
		"capacity": {"capacity": float64(feedBookMaxObservations + 1)},
		"depth":    {"depth": float64(feedBookMaxLevels + 1)},
		"envelope": {"window_ms": float64(900_000), "capacity": float64(4096), "depth": float64(20)},
	} {
		if _, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{l2TestStrategyConfig("l2-"+name, "BTC", params)}}); err == nil {
			t.Fatalf("an out-of-bound %s need must be refused", name)
		}
	}
	var many []StrategyConfig
	for i := 0; i <= feedBookMaxKeys; i++ {
		many = append(many, l2TestStrategyConfig(fmt.Sprintf("l2-%d", i), fmt.Sprintf("C%02d", i), map[string]interface{}{"capacity": float64(16)}))
	}
	if _, err := deriveFeedRequirements(&Config{Strategies: many}); err == nil || !strings.Contains(err.Error(), "16-key bound") {
		t.Fatalf("more book keys than the bound must be refused, got %v", err)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{btc}}); err == nil || !strings.Contains(err.Error(), "cannot supply sealed l2_book observations") {
		t.Fatalf("market_feed=rest with a book need: %v", err)
	}
	if err := validateMarketFeedConfig(&Config{MarketFeed: marketFeedREST, Strategies: []StrategyConfig{oiTestStrategy("plain", "BTC")}}); err != nil && strings.Contains(err.Error(), "l2_book") {
		t.Fatalf("an unrelated REST consumer must not see a book refusal: %v", err)
	}
	for _, mode := range []string{marketFeedWebsocket, marketFeedShared} {
		c := &Config{MarketFeed: mode, Strategies: []StrategyConfig{btc}}
		if mode == marketFeedShared {
			c.SharedMarketFeed = &SharedMarketFeedConfig{PrimarySocket: "/run/go-trader-feed/feed.sock"}
		}
		if err := validateMarketFeedConfig(c); err != nil {
			t.Fatalf("market_feed=%s must derive the book need: %v", mode, err)
		}
	}

	reqEth, _ := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{l2TestStrategyConfig("l2-eth-deep", "ETH", map[string]interface{}{"depth": float64(20), "capacity": float64(100)})}})
	union := unionFeedRequirements([]feedConsumer{{Path: "a", Loaded: true, Req: req}, {Path: "b", Loaded: true, Req: reqEth}})
	if err := bookUnionError(union); err != nil {
		t.Fatalf("compatible consumers must union: %v", err)
	}
	if got := union.Books[l2BookKeyFor("ETH")]; got != contractBookNeed(60_000, 256, 20) {
		t.Fatalf("ETH union %+v", got)
	}
	reqOther := req
	reqOther.Books = map[feedObservationKey]feedBookNeed{l2BookKeyFor("BTC"): {Source: feedBookSource, Contract: "other/v2", TimeSemantics: feedBookTimeSemantics,
		IdentitySemantics: feedBookIdentitySemantics, WindowMs: 60_000, Capacity: 10, Depth: 5}}
	if err := bookUnionError(unionFeedRequirements([]feedConsumer{{Path: "a", Loaded: true, Req: req}, {Path: "c", Loaded: true, Req: reqOther}})); err == nil {
		t.Fatal("consumers with conflicting book contracts must not union")
	}
	full := fullCycleRequirements(union)
	due := cycleRequirementsForDue([]StrategyConfig{btc, btcWide}, req)
	merged := mergeCycleRequirements(due, cycleRequirementsForDue([]StrategyConfig{eth}, req))
	for name, books := range map[string]map[feedObservationKey]feedBookNeed{"full": full.Books, "merged": merged.Books} {
		if books[l2BookKeyFor("BTC")] != want || books[l2BookKeyFor("ETH")] == (feedBookNeed{}) {
			t.Fatalf("%s cycle requirements lost a book dimension: %+v", name, books)
		}
	}
	if due.Books[l2BookKeyFor("BTC")] != want || len(due.Books) != 1 {
		t.Fatalf("due-cycle book needs %+v", due.Books)
	}
	if est := estimateFeedSealBytes(union); est < feedBookSectionBytes(want) {
		t.Fatalf("the seal estimate %d does not count the book need", est)
	}
	served := dropUncollectableObservations(feedSourceREST, []feedConsumer{{Path: "a", Loaded: true, Req: req}})
	if len(served[0].Req.Books) != 0 || served[0].Req.Strategies["l2-btc"].Book != nil || len(served[0].Req.Order) != len(req.Order) || !strings.Contains(served[0].Notice, "l2Book") {
		t.Fatalf("a REST feed source must drop only the book needs: %+v", served[0].Req.Books)
	}
	restSealer := newFeedSealer(newMarketFeedOwner(nil, t.Logf), feedSourceREST, "rest", nil, t.Logf)
	restSealer.setGeneration(unionFeedRequirements(served), []int{60}, time.Now())
	if d := restSealer.describe(); len(d.Books) != 0 {
		t.Fatalf("a REST feed source advertised books: %+v", d.Books)
	}

	owner := newMarketFeedOwner(nil, t.Logf)
	owner.feedMu.Lock()
	owner.applyBookNeedsLocked(req.Books)
	owner.feedMu.Unlock()
	if coins := owner.BookCoins(); strings.Join(coins, ",") != "BTC,ETH" {
		t.Fatalf("owner tracks %v", coins)
	}
	deeper := map[feedObservationKey]feedBookNeed{l2BookKeyFor("BTC"): contractBookNeed(120_000, 256, 20)}
	owner.feedMu.Lock()
	owner.books[l2BookKeyFor("BTC")].Obs = []feedBookObservation{{Seq: owner.nextBookSeq(), EventMs: 1, RecvMs: 2, BidDepth: 20, AskDepth: 20}}
	owner.applyBookNeedsLocked(deeper)
	owner.dropUnneededBooksLocked(deeper)
	st := owner.books[l2BookKeyFor("BTC")]
	reasons := []string{}
	for _, m := range st.Markers {
		reasons = append(reasons, m.Reason)
	}
	owner.feedMu.Unlock()
	if owner.BookCoins()[0] != "BTC" || len(owner.BookCoins()) != 1 || strings.Join(reasons, ",") != "epoch_start,depth_raised" || st.Need.Depth != 20 ||
		len(st.Obs) != 0 || st.Stats.Evicted != 1 {
		t.Fatalf("a generation must release removed keys and warm a raised depth: coins %v markers %v need %+v", owner.BookCoins(), reasons, st.Need)
	}
}

func TestL2BookObservationPipeline(t *testing.T) {
	l2RegisterFixture(t)
	B := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(B - 120_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	l2StubCandles(t, &clockMs)

	btc := l2TestStrategyConfig("l2-btc", "BTC", map[string]interface{}{"window_ms": float64(60_000), "capacity": float64(256), "depth": float64(5)})
	btcWide := l2TestStrategyConfig("l2-btc-wide", "BTC", map[string]interface{}{"window_ms": float64(90_000), "capacity": float64(256), "depth": float64(10)})
	eth := l2TestStrategyConfig("l2-eth", "ETH", map[string]interface{}{"window_ms": float64(120_000), "capacity": float64(8), "depth": float64(5)})
	req, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{btc, btcWide, eth}})
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	req.AccountingCoins = nil
	btcKey, ethKey := l2BookKeyFor("BTC"), l2BookKeyFor("ETH")
	union := unionFeedRequirements([]feedConsumer{{Path: "l2", Loaded: true, Req: req}})
	estimate := estimateFeedSealBytes(union)

	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.corrOffsets = nil
	owner.bookTick = l2TestTickPeriod
	tmp := t.TempDir()
	recDir := filepath.Join(tmp, "recording")
	rec, err := newObservationRecorder(recDir, observationRecorderMeta{RunID: "l2-harness", Coins: []string{"BTC", "ETH"},
		Kinds: []string{feedObservationL2Book}, Segment: 2 * time.Minute, WSURL: fs.url(), StartedAt: clock()}, clock, t.Logf)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	owner.attachRecorder(rec)
	select {
	case <-owner.ApplyGeneration(context.Background(), req):
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not publish")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "l2Book subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `{"coin":"BTC","fast":false,"type":"l2Book"}`) && strings.Contains(subs, `{"coin":"ETH","fast":false,"type":"l2Book"}`)
	})

	stats := func(key feedObservationKey) feedBookStats {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return owner.books[key].Stats
	}
	markers := func(key feedObservationKey) []feedBookMarker {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		return append([]feedBookMarker(nil), owner.books[key].Markers...)
	}
	send := func(coin string, recvMs int64, msg map[string]any) {
		t.Helper()
		key := l2BookKeyFor(coin)
		before := stats(key).Frames
		clockMs.Store(recvMs)
		fs.broadcast(t, msg)
		waitFor(t, fmt.Sprintf("%s book at %d", coin, recvMs), func() bool { return stats(key).Frames > before })
	}
	ack := func(coin string, recvMs int64, extra map[string]any) {
		t.Helper()
		key := l2BookKeyFor(coin)
		before := stats(key).Markers
		owner.feedMu.Lock()
		covered := owner.books[key].Covered
		owner.feedMu.Unlock()
		clockMs.Store(recvMs)
		fs.broadcast(t, l2TestAck(coin, extra))
		if extra != nil && !covered {
			time.Sleep(50 * time.Millisecond)
			return
		}
		waitFor(t, coin+" subscription marker", func() bool { return stats(key).Markers > before })
	}

	send("BTC", B-99_000, l2TestBook("BTC", B-99_300, 0))
	ack("BTC", B-96_000, map[string]any{"nSigFigs": 3})
	if s := stats(btcKey); s.Uncovered != 1 || len(markers(btcKey)) != 1 {
		t.Fatalf("a pre-acknowledgement book is uncovered and an aggregated acknowledgement starts no coverage: %+v %v", s, markers(btcKey))
	}
	ack("BTC", B-95_000, nil)
	ack("ETH", B-95_000, nil)

	tag := 1
	for t0 := B - 90_000; t0 <= B+55_000; t0 += 5_000 {
		switch t0 {
		case B - 60_000:
			send("BTC", t0, l2TestBook("BTC", t0-300, tag))
			send("BTC", t0+1_000, l2TestBook("BTC", t0-300, tag))
			tag++
		case B - 55_000:
			send("BTC", t0, l2TestBook("BTC", t0-300, tag-1))
		case B - 50_000:
			send("BTC", t0, l2TestBook("BTC", t0+feedBookMaxFutureSkewMs, tag))
			tag++
		case B - 40_000:
			send("BTC", t0, l2TestBook("BTC", t0-feedBookMaxLagMs, tag))
			tag++
		default:
			send("BTC", t0, l2TestBook("BTC", t0-300, tag))
			tag++
		}
		if t0 < B-20_000 || t0 > B-5_000 {
			send("ETH", t0+100, l2TestBook("ETH", t0-200, tag))
		}
	}
	if s := stats(btcKey); s.Duplicates != 1 || s.Accepted != 30 || s.Rejected != 0 {
		t.Fatalf("one identical resend, a same-levels new snapshot accepted as distinct, equality bounds accepted: %+v", s)
	}
	if ms := markers(ethKey); ms[len(ms)-1].Reason != feedBookMarkerSilence || ms[len(ms)-1].AtMs != B+100 {
		t.Fatalf("an 20 s ETH gap must write a silence marker at the receipt that ends it: %+v", ms)
	}
	lateEvent := B + 60_200 - 300
	send("BTC", B+60_200, l2TestBook("BTC", lateEvent, tag))
	tag++

	key1 := (B + 60_000) / 1000
	key2 := (B + 180_000) / 1000
	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-l2", clock, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.prepareFn = websocketFeedPrepare(owner)
	sealer.setGeneration(req, []int{60}, time.UnixMilli(B-3_600_000))
	sealer.startServing(time.UnixMilli(B - 3_600_000))
	sockDir, err := os.MkdirTemp("", "l2")
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
	clockMs.Store(B + 60_500)
	sealer.sealOne(ctx, key1)
	sealed1 := append([]byte(nil), sealer.sealBytes(key1)...)
	if len(sealed1) == 0 {
		t.Fatalf("no seal for key %d: %+v", key1, sealer.lookup(key1))
	}

	send("BTC", B+62_000, l2TestBook("BTC", lateEvent, tag+50))
	send("BTC", B+64_000, l2TestBook("BTC", lateEvent-400, tag))
	tag++
	bad := l2TestBook("BTC", B+70_000-300, tag)
	bad["data"].(map[string]any)["levels"].([]any)[0].([]map[string]any)[2]["sz"] = " 1.5"
	send("BTC", B+70_000, bad)
	dots := l2TestBook("BTC", B+71_000-300, tag)
	dots["data"].(map[string]any)["levels"].([]any)[1].([]map[string]any)[3]["px"] = "100004.0.5"
	send("BTC", B+71_000, dots)
	crossed := l2TestBook("BTC", B+72_000-300, tag)
	crossed["data"].(map[string]any)["levels"].([]any)[0].([]map[string]any)[0]["px"] = "100001.5"
	send("BTC", B+72_000, crossed)
	send("BTC", B+74_000, l2TestBook("BTC", B+74_000+feedBookMaxFutureSkewMs+1, tag))
	send("BTC", B+76_000, l2TestBook("BTC", B+76_000-feedBookMaxLagMs-1, tag))
	big := l2TestBook("BTC", B+78_000, tag)
	big["data"].(map[string]any)["pad"] = strings.Repeat("x", feedBookMaxFrameBytes)
	before := len(markers(ethKey))
	clockMs.Store(B + 78_000)
	fs.broadcast(t, big)
	waitFor(t, "oversized frame markers", func() bool { return len(markers(ethKey)) > before })
	before = len(markers(ethKey))
	clockMs.Store(B + 79_000)
	fs.broadcastRaw(t, []byte("{not json"))
	waitFor(t, "malformed envelope markers", func() bool { return len(markers(ethKey)) > before })
	for t0 := B + 80_000; t0 <= B+100_000; t0 += 5_000 {
		send("BTC", t0, l2TestBook("BTC", t0-300, tag))
		tag++
		send("ETH", t0+100, l2TestBook("ETH", t0-200, tag))
	}
	if s := stats(btcKey); s.Conflicts != 1 || s.OutOfOrder != 1 || s.Rejected != 7 {
		t.Fatalf("conflict, out-of-order and seven rejections (whitespace decimal, two dots, crossed, future, stale, oversized, envelope): %+v", s)
	}
	clockMs.Store(B + 111_000)
	owner.TickBooks(clock())
	if ms := markers(btcKey); ms[len(ms)-1].Reason != feedBookMarkerSilence || ms[len(ms)-1].AtMs != B+111_000 {
		t.Fatalf("a timer tick past the gap bound must write one dated silence marker: %+v", ms[len(ms)-1])
	}
	owner.TickBooks(clock())
	earlier := l2TestBook("BTC", B+110_999-300, tag)
	earlier["data"].(map[string]any)["levels"].([]any)[0].([]map[string]any)[1]["sz"] = " 2.5"
	send("BTC", B+110_999, earlier)
	if ms := markers(btcKey); len(ms) < 2 || ms[len(ms)-2].Reason != feedBookMarkerSilence || ms[len(ms)-1].Reason != feedBookMarkerInvalid ||
		ms[len(ms)-1].AtMs != B+111_000 || ms[len(ms)-1].Seq <= ms[len(ms)-2].Seq {
		t.Fatalf("an invalid frame received before the timer silence must take the silence marker's time: %+v", ms[len(ms)-2:])
	}
	clockMs.Store(B + 115_000)
	sessions := fs.sessionCount()
	fs.dropConnections()
	waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
	waitFor(t, "resubscribe", func() bool {
		return strings.Count(strings.Join(fs.subscriptions(), " "), `"coin":"BTC","fast":false,"type":"l2Book"`) > 1
	})
	send("BTC", B+116_000, l2TestBook("BTC", B+116_000-300, tag))
	ack("BTC", B+117_000, nil)
	ack("ETH", B+117_000, nil)
	for t0 := B + 120_000; t0 <= B+175_000; t0 += 5_000 {
		send("BTC", t0, l2TestBook("BTC", t0-300, tag))
		send("ETH", t0+100, l2TestBook("ETH", t0-200, tag))
		tag++
	}
	clockMs.Store(B + 180_500)
	sealer.sealOne(ctx, key2)
	if len(sealer.sealBytes(key2)) == 0 {
		t.Fatalf("no seal for key %d: %+v", key2, sealer.lookup(key2))
	}
	reasons := map[string]int{}
	for _, m := range markers(btcKey) {
		reasons[m.Reason]++
	}
	for reason, want := range map[string]int{feedBookMarkerEpoch: 0, feedBookMarkerSubscribed: 1, feedBookMarkerDisconnected: 1,
		feedBookMarkerConflict: 1, feedBookMarkerOutOfOrder: 1, feedBookMarkerInvalid: 8, feedBookMarkerSilence: 3} {
		if reasons[reason] != want {
			t.Fatalf("BTC marker %s: %d, want %d (%v)", reason, reasons[reason], want, reasons)
		}
	}

	doc1, err := decodeFeedSeal(sealed1, key1)
	if err != nil {
		t.Fatalf("decode seal 1: %v", err)
	}
	if doc1.V != feedSealVersionBooks || len(doc1.L2Books) != 2 || doc1.L2Books[0].Depth != 10 || doc1.L2Books[0].WindowMs != 90_000 {
		t.Fatalf("seal 1 v%d carries %d books %+v", doc1.V, len(doc1.L2Books), doc1.L2Books)
	}
	for _, b := range doc1.L2Books {
		for _, r := range b.Observations {
			if r.RecvMs > key1*1000 || r.EventMs == lateEvent || len(r.Bids) != b.Depth || r.BidDepth != feedBookMaxLevels {
				t.Fatalf("seal 1 holds a post-deadline or wrongly cut record: %+v", r)
			}
		}
	}
	sealer.sealOne(ctx, key1)
	if !bytes.Equal(sealed1, sealer.sealBytes(key1)) {
		t.Fatal("later books or a second seal attempt changed immutable seal bytes")
	}
	owner.feedMu.Lock()
	owner.books[btcKey].Obs[0].Bids[0].Sz = "777"
	owner.feedMu.Unlock()
	if again, err := decodeFeedSeal(sealer.sealBytes(key1), key1); err != nil || again.L2Books[0].Observations[0].Bids[0].Sz == "777" {
		t.Fatalf("mutating the owner's history must not reach sealed bytes: %v", err)
	}
	for name, mutate := range map[string]func(*feedSealDoc){
		"relabeled version": func(d *feedSealDoc) { d.V = feedSealVersionProfile },
		"dropped books":     func(d *feedSealDoc) { d.L2Books = nil },
		"other contract":    func(d *feedSealDoc) { d.L2Books[0].Contract = "other" },
		"over depth":        func(d *feedSealDoc) { d.L2Books[0].Depth = 21 },
		"short identity":    func(d *feedSealDoc) { d.L2Books[0].Observations[1].Identity = strings.Repeat("0", 62) },
		"uppercase identity": func(d *feedSealDoc) {
			d.L2Books[0].Observations[1].Identity = strings.ToUpper(d.L2Books[0].Observations[1].Identity)
		},
		"crossed record": func(d *feedSealDoc) {
			d.L2Books[0].Observations[1].Bids[0].Px = d.L2Books[0].Observations[1].Asks[0].Px
		},
		"reordered records": func(d *feedSealDoc) {
			d.L2Books[0].Observations[0], d.L2Books[0].Observations[1] = d.L2Books[0].Observations[1], d.L2Books[0].Observations[0]
		},
		"unknown marker": func(d *feedSealDoc) { d.L2Books[1].Markers[0].Reason = "nap" },
		"other cutoff":   func(d *feedSealDoc) { d.L2Books[0].CutoffMs++ },
	} {
		cp, _ := decodeFeedSeal(sealed1, key1)
		mutate(cp)
		if blob, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(blob, key1); err == nil {
				t.Fatalf("a tampered book seal (%s) decoded", name)
			}
		}
	}
	sealed2 := append([]byte(nil), sealer.sealBytes(key2)...)
	for name, mutate := range map[string]func(*feedSealL2Book){
		"repeated marker seq":   func(b *feedSealL2Book) { b.Markers[1].Seq = b.Markers[0].Seq },
		"decreasing marker seq": func(b *feedSealL2Book) { b.Markers[0].Seq, b.Markers[1].Seq = b.Markers[1].Seq, b.Markers[0].Seq },
		"decreasing marker time": func(b *feedSealL2Book) {
			b.Markers[1].AtMs = b.Markers[0].AtMs - 1
		},
	} {
		cp, err := decodeFeedSeal(sealed2, key2)
		if err != nil || len(cp.L2Books[0].Markers) < 2 {
			t.Fatalf("seal 2 must decode with at least two BTC markers: %v", err)
		}
		mutate(&cp.L2Books[0])
		if blob, _, err := encodeFeedSeal(cp); err == nil {
			if _, err := decodeFeedSeal(blob, key2); err == nil {
				t.Fatalf("a tampered book seal (%s) decoded", name)
			}
		}
	}
	flipped := append([]byte(nil), sealed1...)
	at := bytes.Index(flipped, []byte(`"identity":"`)) + len(`"identity":"`)
	flipped[at] ^= 1
	if _, err := decodeFeedSeal(flipped, key1); err == nil {
		t.Fatal("a seal with one altered identity byte and its original hash decoded")
	}
	if len(sealed1) > estimate || len(sealed1) > feedSealMaxBytes {
		t.Fatalf("seal 1 is %d bytes; estimate %d, cap %d", len(sealed1), estimate, feedSealMaxBytes)
	}

	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: sock}})
	client.settle, client.prepare = 0, 6*time.Hour
	descs := client.Describe(ctx)
	if descs[0].Err != nil || len(descs[0].Describe.Books) != 2 || descs[0].Header.SealVersion != feedSealVersionBooks || descs[0].Header.PayloadVersion != marketSnapshotVersion {
		t.Fatalf("describe: %+v", descs[0])
	}
	if _, problems := sharedFeedCompatibilityLines("test", descs, req, []int{60}); len(problems) != 0 {
		t.Fatalf("a feed serving the book needs reported coverage problems: %v", problems)
	}
	for name, edit := range map[string]func(*feedDescribeBook){
		"short window":   func(b *feedDescribeBook) { b.WindowMs = 30_000 },
		"small capacity": func(b *feedDescribeBook) { b.Capacity = 4 },
		"shallow":        func(b *feedDescribeBook) { b.Depth = 2 },
		"contract":       func(b *feedDescribeBook) { b.Contract = "hyperliquid-l2book-native-fast/9" },
		"source":         func(b *feedDescribeBook) { b.Source = feedTradeSourceHLWS },
		"time semantics": func(b *feedDescribeBook) { b.TimeSemantics = "receipt_only" },
		"gap bound":      func(b *feedDescribeBook) { b.MaxGapMs = 5_000 },
		"absent":         func(b *feedDescribeBook) { b.Coin = "SOL" },
	} {
		cp := *descs[0].Describe
		cp.Books = append([]feedDescribeBook(nil), cp.Books...)
		for i := range cp.Books {
			if cp.Books[i].Coin == "BTC" {
				edit(&cp.Books[i])
			}
		}
		d := descs[0]
		d.Describe = &cp
		if _, problems := sharedFeedCompatibilityLines("test", []feedEndpointDescription{d}, req, []int{60}); len(problems) == 0 || !strings.Contains(problems[0], "BTC|l2_book") {
			t.Fatalf("a %s book peer must not cover this consumer, got %v", name, problems)
		}
	}
	for _, tc := range []struct {
		sealVersion, payloadVersion int
		compatible                  bool
	}{
		{feedSealVersionProfile, marketSnapshotVersion - 1, false},
		{feedSealVersionBase, marketSnapshotVersion - 1, false},
		{feedSealVersionBase, marketSnapshotVersion, true},
		{feedSealVersionBooks, marketSnapshotVersion, true},
		{feedSealVersionBooks + 1, marketSnapshotVersion, false},
	} {
		peer := l2TestPeer(t, sockDir, tc.sealVersion, tc.payloadVersion)
		_, _, err := client.roundTrip(ctx, peer, feedWireRequest{V: feedWireVersion, Op: feedWireOpDescribe})
		if tc.compatible != (err == nil) {
			t.Fatalf("a seal-v%d payload-v%d peer: compatible=%v, err %v", tc.sealVersion, tc.payloadVersion, tc.compatible, err)
		}
	}

	reqs := cycleRequirementsForDue([]StrategyConfig{btc, btcWide, eth}, req)
	fetch := func(key int64) (*marketFeedContext, sharedFeedFetchReport) {
		t.Helper()
		snap, rep := client.Fetch(ctx, key, reqs)
		if rep.Status != feedFetchSealed {
			t.Fatalf("fetch key %d: %+v", key, rep)
		}
		return &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, SharedKey: key}, rep
	}
	feed1, rep1 := fetch(key1)
	feed2, _ := fetch(key2)
	if len(rep1.Gaps) != 0 || strings.Contains(strings.Join(rep1.Alerts, " "), "COVERAGE GAP") {
		t.Fatalf("an incomplete ETH prefix is an entry hold and no seal coverage gap: %v %v", rep1.Gaps, rep1.Alerts)
	}
	if hold := feed1.holdFor(btc); hold.Held {
		t.Fatalf("BTC with a complete sealed book was held: %+v", hold)
	}
	if hold := feed1.holdFor(eth); !hold.Held || !hold.EntryOnly {
		t.Fatalf("ETH with an incomplete book history must be an entry-only hold: %+v", hold)
	}
	payload1 := l2DecodeEnvelope(t, mustSinglePayload(t, feed1, btc))
	payload2 := l2DecodeEnvelope(t, mustSinglePayload(t, feed2, btc))
	ethPayload := l2DecodeEnvelope(t, mustSinglePayload(t, feed2, eth))
	widePayload := l2DecodeEnvelope(t, mustSinglePayload(t, feed2, btcWide))
	wide := widePayload.L2Books["BTC|l2_book"]
	ordered := false
	for i := 1; i < len(wide.Markers); i++ {
		a, b := wide.Markers[i-1], wide.Markers[i]
		if a.Reason == feedBookMarkerSilence && b.Reason == feedBookMarkerInvalid && a.AtMs == B+111_000 && b.AtMs == B+111_000 && b.Seq > a.Seq {
			ordered = true
		}
	}
	if !wide.Available || !wide.PrefixComplete || !ordered {
		t.Fatalf("the 90 s BTC payload must carry the silence marker and the later-sequenced invalid-frame marker at one time: %+v", wide.Markers)
	}
	if payload1.Version != marketSnapshotVersion {
		t.Fatalf("payload version %d, want %d", payload1.Version, marketSnapshotVersion)
	}
	b1 := payload1.L2Books["BTC|l2_book"]
	if !b1.Available || !b1.PrefixComplete || b1.CutoffMs != key1*1000 || b1.WindowMs != 60_000 || b1.Depth != 5 || b1.Contract != feedBookContract || b1.Instance == "" {
		t.Fatalf("BTC payload book: %+v", b1)
	}
	for _, r := range b1.Observations {
		if r.RecvMs > key1*1000 || r.RecvMs < key1*1000-60_000 || len(r.Bids) != 5 || len(r.Asks) != 5 {
			t.Fatalf("payload record outside the window or depth: %+v", r)
		}
	}
	if e := ethPayload.L2Books["ETH|l2_book"]; !e.Available || e.PrefixComplete || len(e.Observations) > 8 {
		t.Fatalf("the ETH payload must flag its incomplete prefix and stay within capacity: %+v", e.PrefixComplete)
	}
	if _, ok := payload1.Frames["BTC|1m"]; !ok || len(payload1.L2Books) != 1 {
		t.Fatalf("the BTC payload must carry its candle frame and only its own book")
	}
	batch, err := feed1.batchPayload(hlBatchKey{DataPlatform: "hyperliquid", Symbol: "BTC", Timeframe: "1m", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit, ATRMethod: "simple"}, []StrategyConfig{btc, btcWide})
	if err != nil {
		t.Fatalf("batch payload: %v", err)
	}
	if bb := batch.L2Books["BTC|l2_book"]; !bb.Available || bb.WindowMs != 90_000 || bb.Depth != 10 || len(bb.Observations) < len(b1.Observations) {
		t.Fatalf("the batch payload must carry the members' merged need: %+v", bb.WindowMs)
	}
	deep := cycleRequirementsForDue([]StrategyConfig{btc, btcWide, eth}, req)
	deep.Books[btcKey] = contractBookNeed(90_000, 256, 15)
	var deepGaps []string
	for i, key := range []int64{key1, key2} {
		_, rep := client.Fetch(ctx, key, deep)
		alerts := strings.Join(rep.Alerts, " ")
		if len(rep.Gaps) != 1 || !strings.Contains(rep.Gaps[0], "seal depth 10 is below the 15 levels") {
			t.Fatalf("a seal shallower than the need is one coverage gap at key %d: %v", key, rep.Gaps)
		}
		deepGaps = append(deepGaps, rep.Gaps[0])
		if (i == 0) != strings.Contains(alerts, "COVERAGE GAP") {
			t.Fatalf("a depth shortfall alerts once and not again at the next deadline (key %d): %v", key, rep.Alerts)
		}
	}
	if deepGaps[0] != deepGaps[1] {
		t.Fatalf("a depth shortfall gap must keep one text across deadlines: %q vs %q", deepGaps[0], deepGaps[1])
	}

	restart := newMarketFeedOwner(clock, t.Logf)
	restart.feedMu.Lock()
	restart.applyBookNeedsLocked(map[feedObservationKey]feedBookNeed{btcKey: contractBookNeed(60_000, 256, 5)})
	restart.feedMu.Unlock()
	restartSnap := freezeMarketSnapshot(restart, cycleMarketRequirements{Books: map[feedObservationKey]feedBookNeed{btcKey: contractBookNeed(60_000, 256, 5)}},
		"restart/1", time.UnixMilli(B+181_000), time.UnixMilli(B+181_000))
	if rb := restartSnap.books[btcKey]; rb == nil || rb.prefixComplete(60_000) || len(rb.Markers) != 1 || rb.Markers[0].Reason != feedBookMarkerEpoch {
		t.Fatalf("a restarted owner must start a new epoch and warm up: %+v", rb)
	}

	owner.feedMu.Lock()
	btcState, ethState := owner.books[btcKey], owner.books[ethKey]
	owner.feedMu.Unlock()
	owner.feedMu.Lock()
	owner.socketPing = 100 * time.Millisecond
	owner.feedMu.Unlock()
	sessions = fs.sessionCount()
	fs.dropConnections()
	waitFor(t, "reconnect with a short ping period", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
	select {
	case <-owner.ApplyGeneration(context.Background(), feedRequirements{Keys: req.Keys, Order: req.Order, Strategies: req.Strategies,
		Books: map[feedObservationKey]feedBookNeed{btcKey: req.Books[btcKey]}}):
	case <-time.After(5 * time.Second):
		t.Fatal("reduced generation did not publish")
	}
	waitFor(t, "ETH unsubscribe", func() bool {
		return strings.Contains(strings.Join(fs.unsubscriptions(), " "), `{"coin":"ETH","fast":false,"type":"l2Book"}`)
	})

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
	owner.feedMu.Lock()
	finalStats := map[string]feedBookStats{"BTC": btcState.Stats, "ETH": ethState.Stats}
	_, ethTracked := owner.books[ethKey]
	owner.feedMu.Unlock()
	if ethTracked {
		t.Fatal("the reduced generation must release the ETH book state")
	}
	if out := os.Getenv("GO_TRADER_L2BOOK_HARNESS_OUT"); out != "" {
		bn := func(n feedBookNeed) map[string]any {
			return map[string]any{"window_ms": n.WindowMs, "capacity": n.Capacity, "depth": n.Depth}
		}
		cases := []map[string]any{
			{"payload": "payload_1.json", "coin": "BTC", "as_of_ms": B + 60_500, "need": bn(*req.Strategies["l2-btc"].Book), "prefix_complete": true},
			{"payload": "payload_2.json", "coin": "BTC", "as_of_ms": B + 180_500, "need": bn(*req.Strategies["l2-btc"].Book), "prefix_complete": true},
			{"payload": "payload_eth.json", "coin": "ETH", "as_of_ms": B + 180_500, "need": bn(*req.Strategies["l2-eth"].Book), "prefix_complete": false},
			{"payload": "payload_batch.json", "coin": "BTC", "as_of_ms": B + 60_500, "need": bn(req.Books[btcKey]), "prefix_complete": true},
			{"payload": "payload_wide.json", "coin": "BTC", "as_of_ms": B + 180_500, "need": bn(*req.Strategies["l2-btc-wide"].Book), "prefix_complete": true},
		}
		l2WriteHarness(t, out, recDir, map[string]*marketPayload{"payload_1.json": payload1, "payload_2.json": payload2,
			"payload_eth.json": ethPayload, "payload_batch.json": batch, "payload_wide.json": widePayload}, cases, finalStats)
	}
}

func l2WriteHarness(t *testing.T, out, recDir string, payloads map[string]*marketPayload, cases []map[string]any, stats map[string]feedBookStats) {
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
	want := map[string]map[string]uint64{}
	for coin, st := range stats {
		want[coin] = map[string]uint64{"frames": st.Frames, "accepted": st.Accepted, "duplicate": st.Duplicates, "conflict": st.Conflicts,
			"out_of_order": st.OutOfOrder, "rejected": st.Rejected, "uncovered": st.Uncovered, "markers": st.Markers, "evicted": st.Evicted}
	}
	harness := map[string]any{"cases": cases, "expected_stats": want}
	blob, err := json.MarshalIndent(harness, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "harness.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestL2BookCapacityAtTheJointBoundsAndIsolatedOverflow(t *testing.T) {
	l2RegisterFixture(t)
	cutoff := int64(1_800_000_000_000)
	frac := strings.Repeat("1", feedBookDecimalMaxFrac)
	whole := strings.Repeat("9", feedBookDecimalMaxLen-feedBookDecimalMaxFrac-1-2)
	mk := func(depth int) ([]feedBookLevel, []feedBookLevel) {
		bids := make([]feedBookLevel, 0, depth)
		asks := make([]feedBookLevel, 0, depth)
		for i := 0; i < depth; i++ {
			bids = append(bids, feedBookLevel{Px: fmt.Sprintf("%s%02d.%s", whole, 50-i, frac), Sz: whole + "99." + frac, N: feedBookMaxOrders})
			asks = append(asks, feedBookLevel{Px: fmt.Sprintf("%s%02d.%s", whole, 51+i, frac), Sz: whole + "99." + frac, N: feedBookMaxOrders})
		}
		return bids, asks
	}
	bids, asks := mk(feedBookMaxLevels)
	for _, l := range append(append([]feedBookLevel(nil), bids...), asks...) {
		if len(l.Px) != feedBookDecimalMaxLen {
			t.Fatalf("fixture decimal %q is not maximum length", l.Px)
		}
	}
	if err := validateBookSides(bids, asks); err != nil {
		t.Fatalf("maximum book must be valid: %v", err)
	}
	maxCap := func(perKey int) int {
		return (perKey - feedEstimateBookKeyBytes - feedBookMaxMarkers*feedEstimateBookMarkerBytes) / feedBookRecordBytes(feedBookMaxLevels)
	}
	keys := feedBookMaxKeys
	capacity := maxCap(feedBookSealBudget / keys)
	need := contractBookNeed(feedBookMaxWindowMs, capacity, feedBookMaxLevels)
	books := map[feedObservationKey]feedBookNeed{}
	for i := 0; i < keys; i++ {
		books[l2BookKeyFor(fmt.Sprintf("C%02d", i))] = need
	}
	if errs := bookNeedsAdmissionErrors(books); len(errs) != 0 {
		t.Fatalf("the maximum jointly admitted configuration (%d keys x %d records at 20 levels) must be admitted: %v", keys, capacity, errs)
	}
	over := map[feedObservationKey]feedBookNeed{}
	for k, n := range books {
		n.Capacity++
		over[k] = n
	}
	if errs := bookNeedsAdmissionErrors(over); len(errs) == 0 {
		t.Fatal("one record over the joint seal budget must be refused before application")
	}
	fill := func(books map[feedObservationKey]feedBookNeed, records int) *marketSnapshot {
		owner := newMarketFeedOwner(func() time.Time { return time.UnixMilli(cutoff - feedBookMaxWindowMs) }, t.Logf)
		owner.feedMu.Lock()
		owner.applyBookNeedsLocked(books)
		for _, st := range owner.books {
			st.Markers = st.Markers[:0]
			for i := 0; i < feedBookMaxMarkers; i++ {
				owner.addBookMarkerLocked(st, cutoff-feedBookMaxWindowMs+int64(i), feedBookMarkerSubFailed, strings.Repeat("r", feedBookMaxReasonLen), nil)
			}
			for i := 0; i < records; i++ {
				st.Obs = append(st.Obs, feedBookObservation{Seq: owner.nextBookSeq(), EventMs: cutoff - int64(records-i)*100 - 50, RecvMs: cutoff - int64(records-i)*100,
					Session: math.MaxUint32, Identity: strings.Repeat("f", 64), BidDepth: feedBookMaxLevels, AskDepth: feedBookMaxLevels, Bids: bids, Asks: asks})
			}
		}
		owner.feedMu.Unlock()
		return freezeMarketSnapshot(owner, cycleMarketRequirements{Books: books}, feedSealEvaluationID(cutoff/1000), time.UnixMilli(cutoff), time.UnixMilli(cutoff))
	}
	snap := fill(books, capacity)
	for k, b := range snap.books {
		if len(b.Obs) != capacity || len(b.Markers) != feedBookMaxMarkers {
			t.Fatalf("%s froze %d records and %d markers", k.PayloadID(), len(b.Obs), len(b.Markers))
		}
	}
	doc, err := feedSealDocFromSnapshot(snap, cutoff/1000, feedSourceWebsocket, "cap")
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.L2Books {
		for j := range doc.L2Books[i].Observations {
			r := &doc.L2Books[i].Observations[j]
			r.Identity = bookIdentity(feedObservationKey{Host: doc.L2Books[i].Host, Namespace: doc.L2Books[i].Namespace, Coin: doc.L2Books[i].Coin, Kind: doc.L2Books[i].Kind}, r.EventMs, r.Bids, r.Asks)
		}
	}
	blob, _, err := encodeFeedSeal(doc)
	if err != nil {
		t.Fatal(err)
	}
	bookBytes := 0
	for i := range doc.L2Books {
		one, _ := json.Marshal(doc.L2Books[i])
		bookBytes += len(one)
		if len(one) > feedBookSectionBytes(need) {
			t.Fatalf("one sealed book is %d bytes, over its %d-byte estimate", len(one), feedBookSectionBytes(need))
		}
	}
	if bookBytes > feedBookSealBudget || len(blob) > feedSealMaxBytes {
		t.Fatalf("maximum book seal %d bytes (%d of books), budgets %d and %d", len(blob), bookBytes, feedBookSealBudget, feedSealMaxBytes)
	}
	if _, err := decodeFeedSeal(blob, cutoff/1000); err != nil {
		t.Fatalf("decode the maximum seal: %v", err)
	}
	t.Logf("maximum book seal %d bytes (%d of books over %d keys x %d records)", len(blob), bookBytes, keys, capacity)

	oneCap := maxCap(feedBookEnvelopeBudget)
	oneNeed := contractBookNeed(feedBookMaxWindowMs, oneCap, feedBookMaxLevels)
	if errs := bookNeedsAdmissionErrors(map[feedObservationKey]feedBookNeed{l2BookKeyFor("BTC"): oneNeed}); len(errs) != 0 {
		t.Fatalf("the maximum one-key envelope need must be admitted: %v", errs)
	}
	overNeed := oneNeed
	overNeed.Capacity++
	if errs := bookNeedsAdmissionErrors(map[feedObservationKey]feedBookNeed{l2BookKeyFor("BTC"): overNeed}); len(errs) == 0 {
		t.Fatal("one record over the envelope budget must be refused")
	}

	key := l2BookKeyFor("C00")
	need = oneNeed
	snap = fill(map[feedObservationKey]feedBookNeed{key: need}, oneCap)
	payload := &marketPayload{Version: marketSnapshotVersion, SnapshotID: snap.EvaluationID, Frames: map[string]marketFrame{}}
	attachL2BookPayloads(payload, snap, map[feedObservationKey]feedBookNeed{key: need}, feedBookSingleReserveBytes)
	single, err := marketStdinJSON(payload)
	if err != nil || !payload.L2Books[key.PayloadID()].Available || len(single) > marketPayloadMaxBytes {
		t.Fatalf("a maximum admitted single payload must fit: %v (%d bytes)", err, len(single))
	}

	bigKey := feedKeyFor("C00", "1m")
	bars := make([]feedBar, 0, 75_000)
	for i := 0; i < 75_000; i++ {
		open := cutoff - int64(75_000-i)*l2TestBarMs
		bars = append(bars, feedBar{OpenMs: open, CloseMs: open + l2TestBarMs - 1, HasClose: true, Open: 100_000.123, High: 100_010.123, Low: 99_990.123, Close: 100_000.123, Volume: 12.345})
	}
	snap.keys[bigKey] = &marketSnapshotKey{Key: bigKey, IntervalMs: l2TestBarMs, Bars: bars, Readiness: feedKeyReadiness{Key: bigKey, Ready: true, Bars: len(bars), Required: len(bars)}}
	sc := l2TestStrategyConfig("l2-big", "C00", nil)
	entry := feedStrategyRequirement{ID: sc.ID, Signal: bigKey, SignalLookback: len(bars), Coin: "C00", Book: &need}
	feed := &marketFeedContext{Enabled: true, Snapshot: snap, Requirements: feedRequirements{Strategies: map[string]feedStrategyRequirement{sc.ID: entry}}}
	blobBig, err := feed.singleCheckPayload(sc)
	if err != nil {
		t.Fatalf("an over-budget book section must be refused while the candle frame stays usable: %v", err)
	}
	got := l2DecodeEnvelope(t, blobBig)
	if b := got.L2Books[key.PayloadID()]; b.Available || len(b.Observations) != 0 || !strings.Contains(b.Reason, feedBookRefusedCapacity) {
		t.Fatalf("the book section must carry bounded refusal metadata (payload %d bytes): %+v", len(blobBig), b.Reason)
	}
	if f, ok := got.Frames["C00|1m"]; !ok || len(f.Rows) != len(bars) || len(blobBig) > marketPayloadMaxBytes {
		t.Fatalf("the candle frame must survive the book refusal: rows %d, %d bytes", len(f.Rows), len(blobBig))
	}
	if !refuseAllL2BookSections(payload, "test") || payload.L2Books[key.PayloadID()].Available {
		t.Fatal("the batch retry must turn every book section into refusal metadata")
	}

	storm := &feedBookState{Key: l2BookKeyFor("BTC"), Need: contractBookNeed(60_000, 256, 5), Covered: true, RetainedFromMs: cutoff - 60_000}
	stormOwner := newMarketFeedOwner(nil, t.Logf)
	stormOwner.feedMu.Lock()
	for i := 0; i < 3*feedBookMaxMarkers; i++ {
		storm.Obs = append(storm.Obs, feedBookObservation{Seq: stormOwner.nextBookSeq(), EventMs: cutoff - 30_000 + int64(i), RecvMs: cutoff - 30_000 + int64(i)})
		stormOwner.addBookMarkerLocked(storm, cutoff-30_000+int64(i), feedBookMarkerInvalid, "storm", nil)
	}
	stormOwner.feedMu.Unlock()
	last := storm.Markers[len(storm.Markers)-1]
	if len(storm.Markers) > feedBookMaxMarkers || last.Reason != feedBookMarkerEvidenceLoss || storm.RetainedFromMs <= cutoff-30_000 {
		t.Fatalf("a marker storm must coalesce into an evidence-loss barrier and move the complete-history bound: %d markers, last %+v", len(storm.Markers), last)
	}
	for _, o := range storm.Obs {
		if o.RecvMs < storm.RetainedFromMs-1 {
			t.Fatalf("an observation older than the coalesced marker survived: %+v", o)
		}
	}
}

func TestL2BookMixedRecordingKeepsEveryImporter(t *testing.T) {
	B := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(B - 30_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	coins := []string{"BTC", "ETH"}
	kinds := []string{feedObservationOpenInterest, feedObservationTradeFlow, feedObservationL2Book}
	recDir := filepath.Join(t.TempDir(), "recording")
	rec, err := newObservationRecorder(recDir, observationRecorderMeta{RunID: "l2-mixed", Coins: coins, Kinds: kinds,
		Segment: 4 * time.Minute, WSURL: fs.url(), StartedAt: clock()}, clock, t.Logf)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.bookTick = l2TestTickPeriod
	owner.trackObservations(coins, observationRecorderWindow.Milliseconds())
	owner.trackTradeFlows(coins, observationRecorderWindow.Milliseconds())
	owner.attachRecorder(rec)
	owner.trackBooks(coins, recorderBookNeed())
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "every subscription", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		for _, c := range coins {
			for _, want := range []string{`"coin":"` + c + `","type":"activeAssetCtx"`, `"coin":"` + c + `","type":"trades"`, `"coin":"` + c + `","fast":false,"type":"l2Book"`} {
				if !strings.Contains(subs, want) {
					return false
				}
			}
		}
		return true
	})
	counts := func() (uint64, uint64, uint64) {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		var oi, tr, bk uint64
		for _, c := range coins {
			oi += owner.observations[openInterestKeyFor(c)].Stats.Received
			tr += owner.tradeFlows[tradeFlowKeyFor(c)].Stats.Frames
			bk += owner.books[l2BookKeyFor(c)].Stats.Frames
		}
		return oi, tr, bk
	}
	acks := func(at int64) {
		clockMs.Store(at)
		for _, c := range coins {
			fs.broadcast(t, map[string]any{"channel": "subscriptionResponse", "data": map[string]any{
				"method": "subscribe", "subscription": map[string]any{"type": "trades", "coin": c}}})
			fs.broadcast(t, l2TestAck(c, nil))
		}
		waitFor(t, "acknowledgements", func() bool {
			owner.feedMu.Lock()
			defer owner.feedMu.Unlock()
			for _, c := range coins {
				if !owner.books[l2BookKeyFor(c)].Covered || owner.tradeFlows[tradeFlowKeyFor(c)].CoverFromMs == 0 {
					return false
				}
			}
			return true
		})
	}
	acks(B - 20_000)
	tag := 0
	for t0 := B - 15_000; t0 <= B+9*60_000; t0 += 5_000 {
		if t0 == B+4*60_000 {
			clockMs.Store(t0)
			sessions := fs.sessionCount()
			fs.dropConnections()
			waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
			acks(t0 + 1_000)
			continue
		}
		for i, c := range coins {
			recv := t0 + int64(i)*100
			oi0, tr0, bk0 := counts()
			clockMs.Store(recv)
			fs.broadcast(t, map[string]any{"channel": "activeAssetCtx", "data": map[string]any{"coin": c, "ctx": map[string]any{
				"openInterest": fmt.Sprintf("%.3f", 5000+float64(t0-B)/1e6)}}})
			trade := tfScheduledTrade(c, recv-300)
			fs.broadcast(t, map[string]any{"channel": "trades", "data": []any{trade.wire()}})
			fs.broadcast(t, l2TestBook(c, recv-300, tag))
			tag++
			waitFor(t, fmt.Sprintf("%s frames at %d", c, recv), func() bool {
				oi, tr, bk := counts()
				return oi > oi0 && tr > tr0 && bk > bk0
			})
		}
		if t0%60_000 == 0 {
			owner.AdvanceTradeFlows(clock())
		}
	}
	clockMs.Store(B + 9*60_000 + 30_000)
	owner.AdvanceTradeFlows(clock())
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
	blob, err := os.ReadFile(filepath.Join(recDir, "run.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Schema  string            `json:"schema"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(blob, &run); err != nil || run.Schema != observationRunSchemaV3 || run.Sources[feedObservationL2Book] != feedBookSource ||
		run.Sources[feedObservationTradeFlow] != feedTradeSourceHLWS || run.Sources[feedObservationOpenInterest] != feedObservationSourceHLWS {
		t.Fatalf("a mixed run manifest must be schema v3 with every source named: %s (%v)", blob, err)
	}
	if out := os.Getenv("GO_TRADER_L2BOOK_MIXED_OUT"); out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
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
			if err := os.WriteFile(filepath.Join(out, e.Name()), blob, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestL2BookHoldFollowsEachNeedAndWarmupIsNoCoverageGap(t *testing.T) {
	l2RegisterFixture(t)
	T0 := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(T0)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	small := l2TestStrategyConfig("l2-small", "BTC", map[string]interface{}{"window_ms": float64(60_000), "capacity": float64(8), "depth": float64(5)})
	large := l2TestStrategyConfig("l2-large", "BTC", map[string]interface{}{"window_ms": float64(60_000), "capacity": float64(200), "depth": float64(5)})
	req, err := deriveFeedRequirements(&Config{Strategies: []StrategyConfig{small, large}})
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	key := l2BookKeyFor("BTC")
	if n := req.Books[key]; n.Capacity != 200 || n.WindowMs != 60_000 {
		t.Fatalf("the union need must take the larger capacity: %+v", n)
	}
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.feedMu.Lock()
	owner.applyBookNeedsLocked(req.Books)
	owner.feedMu.Unlock()
	owner.SetConnected(true)
	ackRaw, _ := json.Marshal(l2TestAck("BTC", nil))
	owner.IngestBookSubscription("BTC", time.UnixMilli(T0+1_000), ackRaw, map[string]json.RawMessage{
		"type": json.RawMessage(`"l2Book"`), "coin": json.RawMessage(`"BTC"`), "nSigFigs": json.RawMessage(`null`),
		"mantissa": json.RawMessage(`null`), "fast": json.RawMessage(`false`)})
	tag := 0
	frame := func(recvMs int64, mutate func(map[string]any)) {
		msg := l2TestBook("BTC", recvMs-300, tag)
		tag++
		if mutate != nil {
			mutate(msg)
		}
		raw, _ := json.Marshal(msg)
		data, _ := json.Marshal(msg["data"])
		clockMs.Store(recvMs)
		owner.IngestBookFrame(data, time.UnixMilli(recvMs), raw)
	}
	reqs := cycleRequirementsForDue([]StrategyConfig{small, large}, req)
	check := func(label string, cutoffMs int64, wantSmall, wantLarge bool) {
		t.Helper()
		clockMs.Store(cutoffMs)
		snap := freezeMarketSnapshot(owner, reqs, label, time.UnixMilli(cutoffMs), time.UnixMilli(cutoffMs))
		if gaps := applyBookSealCoverage(snap, reqs); len(gaps) != 0 {
			t.Fatalf("%s: an incomplete book prefix is an entry hold and no coverage gap: %v", label, gaps)
		}
		for _, tc := range []struct {
			sc   StrategyConfig
			want bool
		}{{small, wantSmall}, {large, wantLarge}} {
			entry := req.Strategies[tc.sc.ID]
			held, why := snap.bookHold(entry)
			section := snap.l2BookPayload(key, *entry.Book)
			if held != tc.want || !section.Available || held == section.PrefixComplete {
				t.Fatalf("%s: %s held=%v (%s), want %v; its payload prefix_complete=%v must be the inverse", label, tc.sc.ID, held, why, tc.want, section.PrefixComplete)
			}
		}
	}
	for at := T0 + 3_000; at <= T0+18_000; at += 3_000 {
		frame(at, nil)
	}
	check("warm-up 1", T0+20_000, true, true)
	for at := T0 + 21_000; at <= T0+39_000; at += 3_000 {
		frame(at, nil)
	}
	check("warm-up 2", T0+40_000, true, true)
	for at := T0 + 42_000; at <= T0+99_000; at += 3_000 {
		frame(at, nil)
	}
	snap := freezeMarketSnapshot(owner, reqs, "count", time.UnixMilli(T0+100_000), time.UnixMilli(T0+100_000))
	if n := snap.books[key].windowCount(60_000); n != 20 {
		t.Fatalf("the window must hold 20 observations, got %d", n)
	}
	check("union capacity 200 with 20 observations", T0+100_000, true, false)
	for i := 0; i < feedBookMaxMarkers+1; i++ {
		frame(T0+100_100+int64(i)*10, func(m map[string]any) {
			m["data"].(map[string]any)["levels"].([]any)[0].([]map[string]any)[1]["sz"] = " 1.5"
		})
	}
	owner.feedMu.Lock()
	ms := append([]feedBookMarker(nil), owner.books[key].Markers...)
	owner.feedMu.Unlock()
	coalesced := false
	for _, m := range ms {
		coalesced = coalesced || m.Reason == feedBookMarkerEvidenceLoss
	}
	if !coalesced {
		t.Fatalf("a marker storm must coalesce into evidence_loss: %d markers", len(ms))
	}
	check("evidence loss mid-run", T0+102_000, true, true)
}

func TestL2BookResubscribesAStateReplacedBetweenSyncs(t *testing.T) {
	B := (time.Now().UTC().UnixMilli()/3_600_000)*3_600_000 - 2*3_600_000
	var clockMs atomic.Int64
	clockMs.Store(B)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.bookTick = l2TestTickPeriod
	owner.socketPing = 50 * time.Millisecond
	need := contractBookNeed(60_000, 256, 5)
	btcKey, ethKey := l2BookKeyFor("BTC"), l2BookKeyFor("ETH")
	owner.trackBooks([]string{"BTC", "ETH"}, need)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	count := func(list []string, coin string) int {
		return strings.Count(strings.Join(list, " "), `{"coin":"`+coin+`","fast":false,"type":"l2Book"}`)
	}
	covered := func(key feedObservationKey) bool {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		st := owner.books[key]
		return st != nil && st.Covered
	}
	waitFor(t, "first subscriptions", func() bool { return count(fs.subscriptions(), "BTC") == 1 && count(fs.subscriptions(), "ETH") == 1 })
	clockMs.Store(B + 1_000)
	fs.broadcast(t, l2TestAck("BTC", nil))
	fs.broadcast(t, l2TestAck("ETH", nil))
	waitFor(t, "first acknowledgements", func() bool { return covered(btcKey) && covered(ethKey) })
	time.Sleep(300 * time.Millisecond)
	if s, u := fs.subscriptions(), fs.unsubscriptions(); count(s, "BTC") != 1 || count(s, "ETH") != 1 || len(u) != 0 {
		t.Fatalf("an unchanged book state must send no new subscribe across syncs: subs %v unsubs %v", s, u)
	}

	owner.feedMu.Lock()
	oldID := owner.books[btcKey].StateID
	owner.dropUnneededBooksLocked(map[feedObservationKey]feedBookNeed{ethKey: need})
	owner.applyBookNeedsLocked(map[feedObservationKey]feedBookNeed{btcKey: need, ethKey: need})
	owner.subVersion++
	newID := owner.books[btcKey].StateID
	owner.feedMu.Unlock()
	if newID == oldID || covered(btcKey) {
		t.Fatalf("a re-tracked BTC book must be a new, uncovered state: %d -> %d", oldID, newID)
	}
	waitFor(t, "BTC unsubscribe and subscribe again", func() bool {
		return count(fs.unsubscriptions(), "BTC") == 1 && count(fs.subscriptions(), "BTC") == 2
	})
	clockMs.Store(B + 2_000)
	fs.broadcast(t, l2TestAck("BTC", nil))
	waitFor(t, "the replaced BTC state covered", func() bool { return covered(btcKey) })

	owner.feedMu.Lock()
	owner.dropUnneededBooksLocked(map[feedObservationKey]feedBookNeed{btcKey: need})
	owner.subVersion++
	owner.feedMu.Unlock()
	waitFor(t, "ETH unsubscribe", func() bool { return count(fs.unsubscriptions(), "ETH") == 1 })
	time.Sleep(300 * time.Millisecond)
	if s, u := fs.subscriptions(), fs.unsubscriptions(); count(u, "ETH") != 1 || count(s, "ETH") != 1 || count(s, "BTC") != 2 || count(u, "BTC") != 1 {
		t.Fatalf("a drop alone must send exactly one unsubscribe and no subscribe: subs %v unsubs %v", s, u)
	}
}
