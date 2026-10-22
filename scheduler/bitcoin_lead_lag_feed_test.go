package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

const bllHourMs = int64(3_600_000)

func bllStrategies() (bll, momEth, momBtc, solFast StrategyConfig) {
	bll = StrategyConfig{
		ID: "hl-btclag-eth", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args:         []string{"bitcoin_lead_lag", "ETH", "1h", "--mode=paper"},
		OpenStrategy: StrategyRef{Name: "bitcoin_lead_lag", Params: map[string]interface{}{"training_bars": 240.0, "lag_bars": 2.0, "horizon_bars": 4.0}},
		Capital:      1000,
	}
	momEth = StrategyConfig{
		ID: "hl-mom-eth", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", "ETH", "1h", "--mode=paper"}, Capital: 1000,
	}
	momBtc = StrategyConfig{
		ID: "hl-mom-btc", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", "BTC", "1h", "--mode=paper"}, HTFFilter: true, Capital: 1000,
	}
	solFast = StrategyConfig{
		ID: "hl-mom-sol", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", "SOL", "15m", "--mode=paper"}, ClosedBarDecisions: true, Capital: 1000,
	}
	return
}

func bllDerive(t *testing.T, strategies ...StrategyConfig) (feedRequirements, *Config) {
	t.Helper()
	cfg := &Config{IntervalSeconds: 300, MarketFeed: marketFeedWebsocket, Strategies: strategies}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}
	return req, cfg
}

const bllRows = 240 + 2*2 + 2*4 - 2 + 1

func TestBitcoinLeadLagFeedRequirementsAddOnlyTheLeaderFrame(t *testing.T) {
	bll, momEth, momBtc, solFast := bllStrategies()
	req, _ := bllDerive(t, bll, momEth, momBtc, solFast)
	base, _ := bllDerive(t, momEth, momBtc, solFast)

	entry := req.Strategies[bll.ID]
	btc1h, eth1h := feedKeyFor("BTC", "1h"), feedKeyFor("ETH", "1h")
	if !entry.HasLeader || entry.Leader != btc1h || entry.LeaderLookback != bllRows {
		t.Fatalf("leader requirement = %+v, want BTC|1h with %d rows", entry, bllRows)
	}
	if entry.Signal != eth1h || entry.SignalLookback != bllRows || !entry.Timed {
		t.Fatalf("follower requirement = %+v, want ETH|1h with %d timed rows", entry, bllRows)
	}
	if req.Keys[btc1h] != bllRows || req.Keys[eth1h] != bllRows {
		t.Fatalf("subscriptions BTC|1h=%d ETH|1h=%d, want the candidate support %d for both", req.Keys[btc1h], req.Keys[eth1h], bllRows)
	}
	if base.Keys[btc1h] != hlBatchPythonDefaultOhlcvLimit {
		t.Fatalf("baseline BTC|1h lookback = %d", base.Keys[btc1h])
	}
	for _, id := range []string{momEth.ID, momBtc.ID, solFast.ID} {
		if !reflect.DeepEqual(req.Strategies[id], base.Strategies[id]) {
			t.Fatalf("%s requirement changed:\n with=%+v\n base=%+v", id, req.Strategies[id], base.Strategies[id])
		}
		if req.Strategies[id].HasLeader {
			t.Fatalf("%s must carry no leader frame", id)
		}
	}
	for key, lookback := range base.Keys {
		if key == btc1h || key == eth1h {
			continue
		}
		if req.Keys[key] != lookback {
			t.Fatalf("unrelated key %s lookback %d, baseline %d", key, req.Keys[key], lookback)
		}
	}
	if len(req.Keys) != len(base.Keys) {
		t.Fatalf("the candidate added keys beyond its follower and BTC leader: %v vs %v", req.Order, base.Order)
	}

	viaOpen := bll
	viaOpen.ID = "hl-bo-eth"
	viaOpen.Args = []string{"breakout", "ETH", "4h", "--mode=paper"}
	got, _ := bllDerive(t, viaOpen)
	if e := got.Strategies[viaOpen.ID]; !e.HasLeader || e.Leader != feedKeyFor("BTC", "4h") {
		t.Fatalf("an effective open reference must request the BTC leader on its own timeframe: %+v", e)
	}
	asClose := momEth
	asClose.CloseStrategy = &StrategyRef{Name: "bitcoin_lead_lag"}
	got, _ = bllDerive(t, asClose)
	if got.Strategies[asClose.ID].HasLeader {
		t.Fatal("only the effective open name requests the leader frame")
	}

	for label, params := range map[string]map[string]interface{}{
		"training below the floor": {"training_bars": 46.0},
		"odd training":             {"training_bars": 241.0},
		"zero lag":                 {"lag_bars": 0.0},
		"horizon above the cap":    {"horizon_bars": 13.0},
		"fractional lag":           {"lag_bars": 1.5},
		"string horizon":           {"horizon_bars": "4"},
	} {
		bad := bll
		bad.OpenStrategy = StrategyRef{Name: "bitcoin_lead_lag", Params: params}
		cfg := &Config{IntervalSeconds: 300, MarketFeed: marketFeedWebsocket, Strategies: []StrategyConfig{bad}}
		if _, err := deriveFeedRequirements(cfg); err == nil {
			t.Fatalf("%s: unbounded support params must be refused", label)
		}
	}
}

func TestBitcoinLeadLagCycleSelectionDeduplicatesTheLeaderKey(t *testing.T) {
	bll, momEth, momBtc, solFast := bllStrategies()
	req, _ := bllDerive(t, bll, momEth, momBtc, solFast)
	btc1h, eth1h := feedKeyFor("BTC", "1h"), feedKeyFor("ETH", "1h")
	keys := func(due ...StrategyConfig) map[marketFeedKey]int {
		out := map[marketFeedKey]int{}
		reqs := cycleRequirementsForDue(due, req)
		for _, cr := range reqs.Keys {
			if _, dup := out[cr.Key]; dup {
				t.Fatalf("duplicate cycle key %s", cr.Key)
			}
			out[cr.Key] = cr.Required
		}
		return out
	}
	if got := keys(bll); !reflect.DeepEqual(got, map[marketFeedKey]int{eth1h: bllRows, btc1h: bllRows}) {
		t.Fatalf("candidate-only cycle = %v", got)
	}
	if got := keys(momBtc); got[btc1h] != hlBatchPythonDefaultOhlcvLimit {
		t.Fatalf("a BTC strategy due without the candidate keeps its own lookback, got %d", got[btc1h])
	}
	both := keys(bll, momBtc)
	if both[btc1h] != bllRows || len(both) != 3 {
		t.Fatalf("mixed due set = %v, want one BTC|1h key at %d rows plus ETH|1h and the BTC higher timeframe", both, bllRows)
	}
	if got := keys(solFast); len(got) != 1 || got[feedKeyFor("SOL", "15m")] == 0 {
		t.Fatalf("an unrelated strategy's cycle changed: %v", got)
	}
}

func bllBars(coin string, n int, lastOpen int64) []hlCandleRaw {
	base, amp := 100.0, 1.5
	if coin == "ETH" {
		base, amp = 20.0, 0.4
	}
	out := make([]hlCandleRaw, 0, n)
	for i := n - 1; i >= 0; i-- {
		open := lastOpen - int64(i)*bllHourMs
		k := float64(n - i)
		c := base + amp*math.Sin(k/7) + 0.01*k
		o := base + amp*math.Sin((k-1)/7) + 0.01*(k-1)
		out = append(out, hlCandleRaw{
			OpenMs: open, CloseMs: open + bllHourMs - 1, HasClose: i != 0,
			Open: o, High: math.Max(o, c) + 0.1, Low: math.Min(o, c) - 0.1, Close: c, Volume: 5,
		})
	}
	return out
}

func bllOwner(t *testing.T, req feedRequirements, now time.Time) *marketFeedOwner {
	t.Helper()
	owner := newMarketFeedOwner(func() time.Time { return now }, nil)
	lastOpen := now.UnixMilli() / bllHourMs * bllHourMs
	for key, lookback := range req.Keys {
		st := newFeedKeyState(key, mustIntervalMs(t, key.Timeframe), lookback)
		step := st.IntervalMs
		lo := now.UnixMilli() / step * step
		var rows []hlCandleRaw
		if step == bllHourMs {
			rows = bllBars(key.Symbol, lookback+5, lastOpen)
		} else {
			for i := lookback + 4; i >= 0; i-- {
				open := lo - int64(i)*step
				rows = append(rows, hlCandleRaw{OpenMs: open, CloseMs: open + step - 1, HasClose: i != 0,
					Open: 50, High: 51, Low: 49, Close: 50.5, Volume: 3})
			}
		}
		mergeRestRows(st, rows, now.Add(-time.Second))
		st.Status = feedStatusReady
		st.LastRecvAt = now
		owner.keys[key] = st
		owner.published[key] = true
	}
	for _, coin := range req.MidCoins {
		owner.midCoins[coin] = true
		owner.mids[coin] = feedMid{Px: 101, RecvAt: now, Source: "ws"}
	}
	owner.gen = 3
	return owner
}

func bllDecode(t *testing.T, blob []byte) marketPayload {
	t.Helper()
	var env marketStdinEnvelope
	if err := json.Unmarshal(blob, &env); err != nil {
		t.Fatalf("decode stdin envelope: %v", err)
	}
	if env.Market == nil {
		t.Fatal("stdin envelope carries no market payload")
	}
	return *env.Market
}

func bllAssertTimedFrame(t *testing.T, p marketPayload, key marketFeedKey, rows int) marketFrame {
	t.Helper()
	frame, ok := p.Frames[key.PayloadID()]
	if !ok {
		t.Fatalf("payload has no %s frame: %v", key.PayloadID(), bllFrameIDs(p))
	}
	if len(frame.Rows) != rows || frame.Required != rows || !frame.Ready {
		t.Fatalf("%s frame rows=%d required=%d ready=%v, want %d ready rows", key.PayloadID(), len(frame.Rows), frame.Required, frame.Ready, rows)
	}
	if frame.Timing == nil || frame.Timing.Rule != marketFrameTimingRuleHyperliquid || len(frame.Timing.Bars) != rows {
		t.Fatalf("%s frame must carry per-row timing for every row: %+v", key.PayloadID(), frame.Timing)
	}
	return frame
}

func bllFrameIDs(p marketPayload) []string {
	ids := make([]string, 0, len(p.Frames))
	for id := range p.Frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func TestBitcoinLeadLagWebsocketPayloadsCarryBothTimedFramesAndKeepPeers(t *testing.T) {
	bll, momEth, momBtc, solFast := bllStrategies()
	req, cfg := bllDerive(t, bll, momEth, momBtc, solFast)
	base, _ := bllDerive(t, momEth, momBtc, solFast)
	now := time.Unix(1_790_000_000, 0).UTC().Add(90 * time.Second)
	owner := bllOwner(t, req, now)
	due := cfg.Strategies
	snap := sealCycleMarketSnapshot(context.Background(), owner, cycleRequirementsForDue(due, req), "300s/bll", now)
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: 300}
	baseSnap := sealCycleMarketSnapshot(context.Background(), owner, cycleRequirementsForDue(due[1:], base), "300s/bll", now)
	baseFeed := &marketFeedContext{Enabled: true, Requirements: base, Snapshot: baseSnap, Interval: 300}

	blob, err := feed.singleCheckPayload(bll)
	if err != nil {
		t.Fatalf("candidate payload: %v", err)
	}
	p := bllDecode(t, blob)
	eth := bllAssertTimedFrame(t, p, feedKeyFor("ETH", "1h"), bllRows)
	btc := bllAssertTimedFrame(t, p, feedKeyFor("BTC", "1h"), bllRows)
	if len(p.Frames) != 2 || p.DecisionCutoffMs != snap.decisionCutoff().UnixMilli() || p.SnapshotID != "300s/bll" {
		t.Fatalf("candidate payload frames %v cutoff %d snapshot %q", bllFrameIDs(p), p.DecisionCutoffMs, p.SnapshotID)
	}
	if eth.FirstOpenMs != btc.FirstOpenMs || eth.LastOpenMs != btc.LastOpenMs {
		t.Fatalf("follower and leader frames span different bars: ETH %d..%d BTC %d..%d", eth.FirstOpenMs, eth.LastOpenMs, btc.FirstOpenMs, btc.LastOpenMs)
	}

	for _, sc := range []StrategyConfig{momEth, momBtc, solFast} {
		with, err := feed.singleCheckPayload(sc)
		if err != nil {
			t.Fatalf("%s payload: %v", sc.ID, err)
		}
		without, err := baseFeed.singleCheckPayload(sc)
		if err != nil {
			t.Fatalf("%s baseline payload: %v", sc.ID, err)
		}
		if !bytes.Equal(with, without) {
			t.Fatalf("%s payload changed when the candidate joined the config", sc.ID)
		}
	}

	if dir := os.Getenv("GO_TRADER_BLL_PAYLOAD_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "bll_single_payload.json"), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBitcoinLeadLagBatchPayloadRaisesTheFollowerAndAddsTheLeader(t *testing.T) {
	bll, momEth, momBtc, solFast := bllStrategies()
	req, cfg := bllDerive(t, bll, momEth, momBtc, solFast)
	base, _ := bllDerive(t, momEth, momBtc, solFast)
	now := time.Unix(1_790_000_000, 0).UTC().Add(90 * time.Second)
	owner := bllOwner(t, req, now)
	snap := sealCycleMarketSnapshot(context.Background(), owner, cycleRequirementsForDue(cfg.Strategies, req), "300s/bllb", now)
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: 300}
	baseFeed := &marketFeedContext{Enabled: true, Requirements: base, Snapshot: snap, Interval: 300}
	key := hlBatchKey{Symbol: "ETH", Timeframe: "1h", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit}

	mixed, err := feed.batchPayload(key, []StrategyConfig{bll, momEth})
	if err != nil {
		t.Fatalf("mixed batch: %v", err)
	}
	bllAssertTimedFrame(t, *mixed, feedKeyFor("ETH", "1h"), bllRows)
	bllAssertTimedFrame(t, *mixed, feedKeyFor("BTC", "1h"), bllRows)
	if len(mixed.Frames) != 2 || mixed.DecisionCutoffMs == 0 {
		t.Fatalf("mixed batch frames %v cutoff %d", bllFrameIDs(*mixed), mixed.DecisionCutoffMs)
	}

	peerOnly, err := feed.batchPayload(key, []StrategyConfig{momEth})
	if err != nil {
		t.Fatal(err)
	}
	peerBase, err := baseFeed.batchPayload(key, []StrategyConfig{momEth})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(peerOnly)
	b, _ := json.Marshal(peerBase)
	if !bytes.Equal(a, b) {
		t.Fatal("a batch without the candidate must keep its payload byte-identical")
	}
	if f := peerOnly.Frames[feedKeyFor("ETH", "1h").PayloadID()]; len(f.Rows) != hlBatchPythonDefaultOhlcvLimit || f.Timing != nil || len(peerOnly.Frames) != 1 {
		t.Fatalf("peer-only batch frame rows=%d timing=%v frames=%v", len(f.Rows), f.Timing != nil, bllFrameIDs(*peerOnly))
	}

	btcKey := hlBatchKey{Symbol: "BTC", Timeframe: "1h", OhlcvLimit: hlBatchPythonDefaultOhlcvLimit}
	btcFollower := bll
	btcFollower.ID = "hl-btclag-btc"
	btcFollower.Args = []string{"bitcoin_lead_lag", "BTC", "1h", "--mode=paper"}
	req2, _ := bllDerive(t, btcFollower, momBtc)
	feed2 := &marketFeedContext{Enabled: true, Requirements: req2, Snapshot: snap, Interval: 300}
	self, err := feed2.batchPayload(btcKey, []StrategyConfig{btcFollower, momBtc})
	if err != nil {
		t.Fatalf("BTC follower batch: %v", err)
	}
	bllAssertTimedFrame(t, *self, feedKeyFor("BTC", "1h"), bllRows)
	single, err := feed2.singleCheckPayload(btcFollower)
	if err != nil {
		t.Fatal(err)
	}
	if p := bllDecode(t, single); len(p.Frames) != 1 {
		t.Fatalf("a BTC follower deduplicates its leader key into one frame, got %v", bllFrameIDs(p))
	}
}

func TestBitcoinLeadLagMissingLeaderDegradesTheWholeEvaluationAndKeepsProtection(t *testing.T) {
	bll, momEth, _, _ := bllStrategies()
	req, cfg := bllDerive(t, bll, momEth)
	now := time.Unix(1_790_000_000, 0).UTC().Add(90 * time.Second)
	owner := bllOwner(t, req, now)
	snap := sealCycleMarketSnapshot(context.Background(), owner, cycleRequirementsForDue(cfg.Strategies, req), "300s/bllm", now)
	leader := feedKeyFor("BTC", "1h")
	delete(snap.keys, leader)
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: 300}

	hold := feed.holdFor(bll)
	if !hold.Held || hold.Key != leader {
		t.Fatalf("a missing leader frame must hold the candidate on the leader key: %+v", hold)
	}
	if _, err := feed.singleCheckPayload(bll); err == nil {
		t.Fatal("the payload builder must refuse a requested leader frame that is not ready")
	}
	if held, _ := feed.feedHoldsSignal(momEth); held {
		t.Fatal("a peer on the follower coin keeps its own payload when only the leader frame is missing")
	}
	if _, err := feed.singleCheckPayload(momEth); err != nil {
		t.Fatalf("peer payload: %v", err)
	}

	origPlain := runHyperliquidCheckFn
	origStdin := runHyperliquidCheckWithStdinFn
	spawns := 0
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		spawns++
		return nil, "", nil
	}
	runHyperliquidCheckWithStdinFn = func(string, []string, []byte) (*HyperliquidResult, string, error) {
		spawns++
		return nil, "", nil
	}
	t.Cleanup(func() {
		runHyperliquidCheckFn = origPlain
		runHyperliquidCheckWithStdinFn = origStdin
	})
	result, _, price, ok := runHyperliquidCheck(&bll, map[string]float64{"ETH": 101}, PositionCtx{}, nil, "simple", nil, hlBatchTestLogger(), nil, feed)
	if !ok || spawns != 0 {
		t.Fatalf("a whole-payload failure returns a degraded result without spawning a check: ok=%v spawns=%d", ok, spawns)
	}
	if result.Signal != 0 || result.CloseFraction != 0 || result.Degraded == "" || price != 101 {
		t.Fatalf("degraded result must carry no candle-derived signal or close and price from the verified mid: %+v price %v", result, price)
	}

	delete(snap.mids, "ETH")
	_, _, _, ok = runHyperliquidCheck(&bll, map[string]float64{}, PositionCtx{}, nil, "simple", nil, hlBatchTestLogger(), nil, feed)
	if ok || spawns != 0 {
		t.Fatalf("without a verified mark the degraded evaluation is skipped: ok=%v spawns=%d", ok, spawns)
	}
}

func TestBitcoinLeadLagSharedCycleDeliversTheLeaderThroughTheSeal(t *testing.T) {
	bll, momEth, momBtc, _ := bllStrategies()
	req, cfg := bllDerive(t, bll, momEth, momBtc)
	now := time.Now().UTC()
	lastOpen := now.UnixMilli() / bllHourMs * bllHourMs
	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		if interval != "1h" {
			step := mustIntervalMs(t, interval)
			lo := now.UnixMilli() / step * step
			var out []hlCandleRaw
			for i := 79; i >= 0; i-- {
				open := lo - int64(i)*step
				out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + step - 1, HasClose: i != 0, Open: 50, High: 51, Low: 49, Close: 50.5, Volume: 3})
			}
			return out, nil
		}
		return bllBars(coin, bllRows+20, lastOpen), nil
	})
	union := unionFeedRequirements([]feedConsumer{{Loaded: true, Req: req}})
	leader := feedKeyFor("BTC", "1h")
	if union.Keys[leader] != bllRows || !union.SignalKeys[leader] {
		t.Fatalf("the feed union must subscribe and prioritise the leader key: lookback %d signal %v", union.Keys[leader], union.SignalKeys[leader])
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := newMarketFeedOwner(nil, t.Logf)
	select {
	case <-owner.ApplyGeneration(ctx, union):
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not publish")
	}
	owner.IngestMids(map[string]float64{"BTC": 125.5, "ETH": 12.75}, now, "ws")
	sealer := newFeedSealer(owner, feedSourceWebsocket, "test-instance", nil, t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.setGeneration(union, []int{60}, now.Add(-10*time.Minute))
	sealer.startServing(now.Add(-10 * time.Minute))
	key := now.Unix() / 60 * 60
	sealer.sealOne(ctx, key)

	dir, err := os.MkdirTemp("", "bll")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	srv, err := listenFeedSocket(socket, sealer, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	go srv.serve()
	defer srv.close()
	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: socket}})
	client.settle, client.prepare = 0, time.Hour
	snap, report := client.Fetch(ctx, key, cycleRequirementsForDue(cfg.Strategies, req))
	if report.Status != feedFetchSealed || len(report.Gaps) != 0 {
		t.Fatalf("shared fetch: %+v", report)
	}
	shared := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, SharedKey: key}
	blob, err := shared.singleCheckPayload(bll)
	if err != nil {
		t.Fatalf("shared candidate payload: %v", err)
	}
	p := bllDecode(t, blob)
	sharedEth := bllAssertTimedFrame(t, p, feedKeyFor("ETH", "1h"), bllRows)
	sharedBtc := bllAssertTimedFrame(t, p, leader, bllRows)

	local := freezeMarketSnapshot(owner, cycleRequirementsForDue(cfg.Strategies, req), "local", snap.SealedAt, snap.decisionCutoff())
	localFeed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: local}
	lp := bllDecode(t, mustSinglePayload(t, localFeed, bll))
	for id, frame := range map[string]marketFrame{"ETH|1h": sharedEth, "BTC|1h": sharedBtc} {
		lf := lp.Frames[id]
		if !reflect.DeepEqual(lf.Rows, frame.Rows) || !reflect.DeepEqual(bllTimingInts(lf.Timing), bllTimingInts(frame.Timing)) {
			t.Fatalf("%s rows or timing differ between the seal and the owner it sealed", id)
		}
	}
	peer, err := shared.singleCheckPayload(momBtc)
	if err != nil {
		t.Fatal(err)
	}
	if pp := bllDecode(t, peer); len(pp.Frames[leader.PayloadID()].Rows) != hlBatchPythonDefaultOhlcvLimit {
		t.Fatalf("a BTC peer keeps its own lookback in the shared cycle, got %d rows", len(pp.Frames[leader.PayloadID()].Rows))
	}
}

func mustSinglePayload(t *testing.T, feed *marketFeedContext, sc StrategyConfig) []byte {
	t.Helper()
	blob, err := feed.singleCheckPayload(sc)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func bllTimingInts(timing *marketFrameTiming) [][3]string {
	if timing == nil {
		return nil
	}
	out := make([][3]string, 0, len(timing.Bars))
	for _, b := range timing.Bars {
		blob, _ := json.Marshal(b)
		var row [3]json.RawMessage
		_ = json.Unmarshal(blob, &row)
		out = append(out, [3]string{string(row[0]), string(row[1]), string(row[2])})
	}
	return out
}

func TestBitcoinLeadLagRegistrationTables(t *testing.T) {
	name := bitcoinLeadLagStrategyName
	if !openStrategyRegisteredFor(name, "futures") || openStrategyRegisteredFor(name, "spot") {
		t.Fatal("bitcoin_lead_lag must be registered for futures only")
	}
	ref := "backtest/candidates/bitcoin_lead_lag_1640/REPORT.md"
	if ev, ok := noEdgeEvidenceFor(name); !ok || ev.Ref != ref {
		t.Fatalf("no-edge evidence = %+v, %v", ev, ok)
	}
	want := map[string]string{
		cumulativeVolumeDeltaStrategyName: "backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md",
		macroEventBreakoutStrategyName:    "backtest/candidates/macro_event_breakout_1654/REPORT.md",
		volumeProfileReclaimStrategyName:  "backtest/candidates/volume_profile_reclaim_1665/REPORT.md",
		orderBookImbalanceStrategyName:    "backtest/candidates/order_book_imbalance_1635/REPORT.md",
		name:                              ref,
		perpetualPremiumStrategyName:      "backtest/candidates/perpetual_premium_reversion_1639/REPORT.md",
	}
	if !reflect.DeepEqual(productionRefusedStrategies, want) {
		t.Fatalf("production-refused table = %v", productionRefusedStrategies)
	}
	if _, short := knownShortNames[name]; short {
		t.Fatal("a production-refused candidate gets no scheduler short name before its promotion")
	}
	if isBidirectionalPerpsStrategy(name) {
		t.Fatal("a production-refused candidate stays out of the bidirectional map before its promotion")
	}
	if _, frozen := v20StampEligibleStrategies[name]; frozen {
		t.Fatal("the candidate must not join the frozen v20 auto-acknowledgement set")
	}
	if fixedIntervalStrategyDefaults[name] != bllHourMs {
		t.Fatalf("fixed interval default = %d", fixedIntervalStrategyDefaults[name])
	}
	if _, _, err := buildAddStrategyEntry(name, "hyperliquid", "eth"); err == nil {
		t.Fatal("the add-strategy command must refuse the candidate")
	}
}
