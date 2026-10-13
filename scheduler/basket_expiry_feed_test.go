package main

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type basketExpiryFeedRig struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	union   feedRequirements
	owners  map[string]*marketFeedOwner
	sealers map[string]*feedSealer
	servers map[string]*feedUnixServer
	client  *sharedFeedClient
	dials   atomic.Int64
	private atomic.Int64
}

func basketExpiryFeedUnion(coins []string) feedRequirements {
	u := feedRequirements{Keys: map[marketFeedKey]int{}, MidCoins: append([]string(nil), coins...), Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}}
	u.finalize()
	return u
}

func newBasketExpiryFeedRig(t *testing.T, startAt time.Time) *basketExpiryFeedRig {
	t.Helper()
	r := &basketExpiryFeedRig{t: t, owners: map[string]*marketFeedOwner{}, sealers: map[string]*feedSealer{}, servers: map[string]*feedUnixServer{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.ctx = ctx
	dir, err := os.MkdirTemp("", "gtx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r.dir = dir
	origMids := fetchHyperliquidMidsCtxFn
	fetchHyperliquidMidsCtxFn = func(ctx context.Context, coins []string) (map[string]float64, error) {
		r.private.Add(1)
		return nil, errors.New("the expiry path must never fetch a private price")
	}
	origCandles := fetchHyperliquidCandleSnapshotFn
	fetchHyperliquidCandleSnapshotFn = func(ctx context.Context, coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		r.private.Add(1)
		return nil, errors.New("the expiry path needs no candle")
	}
	t.Cleanup(func() {
		fetchHyperliquidMidsCtxFn = origMids
		fetchHyperliquidCandleSnapshotFn = origCandles
	})
	coins := []string{"AVAX", "BTC", "ETH", "SOL"}
	r.union = basketExpiryFeedUnion(coins)
	for _, name := range []string{"primary", "backup"} {
		owner := newMarketFeedOwner(nil, t.Logf)
		select {
		case <-owner.ApplyGeneration(ctx, r.union):
		case <-time.After(5 * time.Second):
			t.Fatalf("%s generation did not publish", name)
		}
		sealer := newFeedSealer(owner, feedSourceWebsocket, "inst-"+name, nil, t.Logf)
		sealer.settle = 0
		sealer.prepare = time.Hour
		sealer.prepareFn = websocketFeedPrepare(owner)
		sealer.setGeneration(r.union, []int{60}, startAt)
		sealer.startServing(startAt)
		r.owners[name], r.sealers[name] = owner, sealer
		r.listen(name)
	}
	r.client = newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{
		{Name: "primary", Socket: filepath.Join(dir, "primary.sock")},
		{Name: "backup", Socket: filepath.Join(dir, "backup.sock")},
	})
	r.client.settle, r.client.prepare = 0, time.Hour
	dial := r.client.dial
	r.client.dial = func(ctx context.Context, path string) (net.Conn, error) {
		r.dials.Add(1)
		return dial(ctx, path)
	}
	return r
}

func (r *basketExpiryFeedRig) listen(name string) {
	r.t.Helper()
	srv, err := listenFeedSocket(filepath.Join(r.dir, name+".sock"), r.sealers[name], r.t.Logf)
	if err != nil {
		r.t.Fatal(err)
	}
	go srv.serve()
	r.servers[name] = srv
	r.t.Cleanup(srv.close)
}

func (r *basketExpiryFeedRig) seal(name string, key int64, marks map[string]float64) {
	r.t.Helper()
	r.owners[name].IngestMids(marks, time.Now().UTC(), "ws")
	r.sealers[name].sealOne(r.ctx, key)
	if h := r.sealers[name].lookup(key); h.Status != feedWireStatusSealed {
		r.t.Fatalf("%s did not seal key %d: %+v", name, key, h)
	}
}

func (r *basketExpiryFeedRig) feed() *basketExpiryFeed {
	return &basketExpiryFeed{client: r.client, sched: &sharedFeedSchedule{Cadences: []int{60}, Window: r.client.giveUpWindow()}}
}

func basketExpiredEnv(t *testing.T) (*basketTestEnv, time.Time) {
	t.Helper()
	cfg := basketHorizonConfig(t, 90, nil)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketTestDeadline().Add(-24 * time.Hour)
	opened := env.openMembers(deadline)
	return env, opened.Add(90 * time.Second)
}

func (e *basketTestEnv) allFlat() bool {
	for _, m := range basketTestMembers {
		if e.signedQty(m.ID, m.Coin) != 0 {
			return false
		}
	}
	return true
}

func TestBasketExpiryMarksFromARealSharedFeedService(t *testing.T) {
	now := time.Now().UTC()
	key := now.Unix() / 60 * 60
	rig := newBasketExpiryFeedRig(t, now.Add(-10*time.Minute))
	coins := []string{"AVAX", "BTC", "ETH", "SOL"}

	env, expiry := basketExpiredEnv(t)
	if expiry.Unix()%60 == 0 {
		t.Fatal("the fixture expiry must sit off the feed grid")
	}
	if got := env.bc.expiryMarkCoins(now); strings.Join(got, ",") != strings.Join(coins, ",") {
		t.Fatalf("expiry mark coins %v, want every held member", got)
	}

	stale := &basketExpiryFeed{client: rig.client, sched: &sharedFeedSchedule{Cadences: []int{60}, Window: time.Second}}
	staleNow := time.Unix(key, 0).UTC().Add(30 * time.Second)
	got := stale.marks(rig.ctx, staleNow, coins, nil, 0)
	if got.Snapshot != nil || got.Report != nil || rig.dials.Load() != 0 || !got.RetryAt.Equal(time.Unix(key+60, 0).UTC()) {
		t.Fatalf("a stale schedule key must send no request and wait for the next key: %+v dials %d", got, rig.dials.Load())
	}

	rig.servers["primary"].close()
	rig.servers["backup"].close()
	feed := rig.feed()
	got = feed.marks(rig.ctx, now, coins, nil, 0)
	if got.Snapshot != nil || got.Report == nil || got.Report.Status != feedFetchDegraded || got.Report.Key != key {
		t.Fatalf("a key no endpoint serves must not serve marks: %+v", got.Report)
	}
	if !got.RetryAt.Equal(time.Unix(key+60, 0).UTC()) {
		t.Fatalf("a failed key must retry at the next supported key, got %s", got.RetryAt)
	}
	dials := rig.dials.Load()
	if again := feed.marks(rig.ctx, now, coins, nil, 0); again.Report != nil || rig.dials.Load() != dials {
		t.Fatal("a key already tried must never be requested again")
	}
	env.stepAt(now, nil, true, got.RetryAt)
	if env.allFlat() {
		t.Fatal("no fresh seal may close the basket")
	}
	for _, m := range basketTestMembers {
		if l := env.rt().Lifecycle[m.ID]; l.RetryAtMs != got.RetryAt.UnixMilli() {
			t.Fatalf("%s retry %d, want the next supported key %d", m.ID, l.RetryAtMs, got.RetryAt.UnixMilli())
		}
	}
	if d := env.bc.wakeDelay(now); d != time.Unix(key+60, 0).Sub(now) {
		t.Fatalf("a blocked obligation must wake at the next key, got %s", d)
	}

	rig.listen("primary")
	rig.listen("backup")
	next := key + 60
	nextAt := time.Unix(next, 0).UTC().Add(time.Second)
	partial := basketMarksWithout("AVAX")
	rig.seal("primary", next, partial)
	got = feed.marks(rig.ctx, nextAt, coins, nil, 0)
	if got.Snapshot != nil || got.Report == nil || got.Report.Status != feedFetchSealed || got.Report.Key != next {
		t.Fatalf("a seal without every held-member mark must not serve: %+v", got.Report)
	}
	env.stepAt(nextAt, got.Snapshot, true, got.RetryAt)
	if env.allFlat() {
		t.Fatal("a missing held-member mark must hold the close")
	}

	third := next + 60
	thirdAt := time.Unix(third, 0).UTC().Add(time.Second)
	rig.seal("primary", third, basketTestMarks())
	got = feed.marks(rig.ctx, thirdAt, coins, nil, 0)
	if got.Snapshot == nil || got.Report.Endpoint != "primary" || got.Report.Key != third || got.Report.Key == expiry.Unix() {
		t.Fatalf("a fresh supported key must serve marks-only: %+v", got.Report)
	}
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), nil)
	env.stepAt(thirdAt, got.Snapshot, true, got.RetryAt)
	if !env.allFlat() {
		t.Fatal("the overdue expiry after restart must close on the fresh sealed marks")
	}
	if rig.private.Load() != 0 {
		t.Fatalf("the expiry path made %d private price or candle requests", rig.private.Load())
	}
}

func TestBasketExpiryMarksFailOverToTheBackupAndHoldWhenBothFail(t *testing.T) {
	now := time.Now().UTC()
	key := now.Unix() / 60 * 60
	rig := newBasketExpiryFeedRig(t, now.Add(-10*time.Minute))
	coins := []string{"AVAX", "BTC", "ETH", "SOL"}
	rig.seal("backup", key, basketTestMarks())
	rig.servers["primary"].close()

	env, _ := basketExpiredEnv(t)
	got := rig.feed().marks(rig.ctx, now, coins, nil, 0)
	if got.Snapshot == nil || got.Report.Endpoint != "backup" {
		t.Fatalf("the backup must serve when the primary is down: %+v", got.Report)
	}
	env.stepAt(now, got.Snapshot, true, got.RetryAt)
	if !env.allFlat() {
		t.Fatal("expiry must close on the backup seal")
	}

	env2, _ := basketExpiredEnv(t)
	rig.servers["backup"].close()
	got = rig.feed().marks(rig.ctx, now, coins, nil, 0)
	if got.Snapshot != nil || got.Report.Status != feedFetchDegraded {
		t.Fatalf("both endpoints down must degrade: %+v", got.Report)
	}
	env2.stepAt(now, got.Snapshot, true, got.RetryAt)
	if env2.allFlat() {
		t.Fatal("a degraded seal must hold the close")
	}
	for _, m := range basketTestMembers {
		if l := env2.rt().Lifecycle[m.ID]; !l.active() || l.RetryAtMs != got.RetryAt.UnixMilli() {
			t.Fatalf("%s must keep its obligation and retry at the next key: %+v", m.ID, l)
		}
	}
	if rig.private.Load() != 0 {
		t.Fatalf("the expiry path made %d private price or candle requests", rig.private.Load())
	}
}

func TestBasketExpiryMarksRefuseEvictedAndPreStartKeys(t *testing.T) {
	now := time.Now().UTC()
	key := now.Unix() / 60 * 60
	coins := []string{"AVAX", "BTC", "ETH", "SOL"}
	rig := newBasketExpiryFeedRig(t, now.Add(-30*time.Minute))
	old := key - 5*60
	for k := old; k <= key; k += 60 {
		rig.seal("primary", k, basketTestMarks())
		rig.seal("backup", k, basketTestMarks())
	}
	if h := rig.sealers["primary"].lookup(old); h.Status != feedWireStatusUnavailable {
		t.Fatalf("key %d should be evicted: %+v", old, h)
	}
	got := rig.feed().marks(rig.ctx, time.Unix(old, 0).UTC().Add(5*time.Second), coins, nil, 0)
	if got.Snapshot != nil || got.Report == nil || got.Report.Key != old || got.Report.Status != feedFetchDegraded {
		t.Fatalf("an evicted key must never serve marks: %+v", got.Report)
	}

	late := newBasketExpiryFeedRig(t, now.Add(10*time.Minute))
	got = late.feed().marks(late.ctx, now, coins, nil, 0)
	if got.Snapshot != nil || got.Report == nil || got.Report.Status != feedFetchDegraded {
		t.Fatalf("a key before the feed's first deadline must never serve marks: %+v", got.Report)
	}
	for _, a := range got.Report.Attempts {
		if !strings.Contains(a, "unavailable") {
			t.Fatalf("a pre-start key must read unavailable, got %q", a)
		}
	}
	if rig.private.Load()+late.private.Load() != 0 {
		t.Fatal("the expiry path made a private price or candle request")
	}
}

func TestBasketExpiryMarksFromTheWebsocketMarksOnlySeal(t *testing.T) {
	coins := []string{"AVAX", "BTC", "ETH", "SOL"}
	newOwner := func(marks map[string]float64) *marketFeedOwner {
		t.Helper()
		owner := newMarketFeedOwner(nil, t.Logf)
		select {
		case <-owner.ApplyGeneration(context.Background(), basketExpiryFeedUnion(coins)):
		case <-time.After(5 * time.Second):
			t.Fatal("generation did not publish")
		}
		owner.IngestMids(marks, time.Now().UTC(), "ws")
		return owner
	}
	var private atomic.Int64
	origMids := fetchHyperliquidMidsCtxFn
	fetchHyperliquidMidsCtxFn = func(ctx context.Context, c []string) (map[string]float64, error) {
		private.Add(1)
		return nil, errors.New("no private price")
	}
	t.Cleanup(func() { fetchHyperliquidMidsCtxFn = origMids })

	env, _ := basketExpiredEnv(t)
	now := time.Now().UTC()
	missing := sealCycleMarketSnapshot(context.Background(), newOwner(basketMarksWithout("SOL")), cycleMarketRequirements{Coins: coins}, "test/basket-marks", now)
	env.stepAt(now, missing, true, time.Time{})
	if env.allFlat() {
		t.Fatal("a websocket seal without a held-member mark must hold the close")
	}
	later := now.Add(basketExpiryRetryFloor + time.Second)
	snap := sealCycleMarketSnapshot(context.Background(), newOwner(basketTestMarks()), cycleMarketRequirements{Coins: coins}, "test/basket-marks", time.Now().UTC())
	env.stepAt(later, snap, true, time.Time{})
	if !env.allFlat() {
		t.Fatal("an expiry-only cycle must close on the websocket marks-only seal")
	}
	if private.Load() != 0 {
		t.Fatal("the websocket expiry path made a private price request")
	}
}

func TestBasketExpiryEarlyPassClosesOnTheCycleSealWithoutRanking(t *testing.T) {
	env, expiry := basketExpiredEnv(t)
	at := expiry.Add(time.Minute)
	snap := basketTestSnapshot(at, 60, basketTestMarks(), 5)
	before := len(env.rt().Rebalances)
	env.bc.stepExpiry(basketCycleInputs{Now: at, Snapshot: snap, SaveBlocked: true})
	if env.allFlat() {
		t.Fatal("the early pass must not act while saves are blocked")
	}
	env.bc.stepExpiry(basketCycleInputs{Now: at, Snapshot: snap})
	if !env.allFlat() {
		t.Fatal("the early pass must close due members on a fresh cycle seal")
	}
	if len(env.rt().Rebalances) != before || env.rt().rank != nil {
		t.Fatal("the early pass must not reserve or rank a deadline")
	}
	for _, m := range basketTestMembers {
		if l, _, _ := env.lifecycleRow(m.ID); l.active() {
			t.Fatalf("%s lifecycle still active after the early pass: %+v", m.ID, l)
		}
	}
}

func basketTwoLegBoundEnv(t *testing.T) (*basketTestEnv, *scriptedBasketVenue, time.Time) {
	t.Helper()
	weights := map[string]float64{"BTC": 0.25, "ETH": -0.25, "SOL": 0, "AVAX": 0}
	cfg := loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["holding_horizon_seconds"] = 8 * 3600
		b["params"] = map[string]interface{}{"weights": map[string]interface{}{"BTC": 0.25, "ETH": -0.25, "SOL": 0, "AVAX": 0}}
	})
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(weights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	var firstEntry time.Time
	for h := 0; h < 6; h++ {
		candle := deadline.Add(time.Duration(h) * time.Hour)
		now := candle.Add(time.Minute)
		env.cycle(now, candle, basketTestSnapshot(candle, 60, basketTestMarks(), 1), true)
		if firstEntry.IsZero() && env.signedQty("hl-bsk-btc", "BTC") != 0 {
			firstEntry = now
		}
	}
	if rb := env.rt().Rebalances[deadline.UnixMilli()]; rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the two-leg rebalance did not complete under the $400 net bound: %+v", rb)
	}
	return env, venue, firstEntry
}

func basketColdLotCache(t *testing.T, fail *atomic.Bool, fetches *atomic.Int64) *hlLotMetadataCache {
	t.Helper()
	c := newHLLotMetadataCache(nil)
	c.logf = func(string, ...any) {}
	var entries []string
	for _, coin := range []string{"AVAX", "BTC", "DOGE", "ETH", "SOL"} {
		entries = append(entries, `{"name":"`+coin+`","szDecimals":`+strconv.Itoa(basketTestLots[coin])+`}`)
	}
	body := hlMetaBody(entries...)
	c.fetch = func(context.Context, string) ([]byte, error) {
		fetches.Add(1)
		if fail.Load() {
			return nil, errors.New("meta endpoint unavailable")
		}
		return []byte(body), nil
	}
	return c
}

func TestBasketExpiryEarlyPassLoadsLotSizesBeforeSizingATranche(t *testing.T) {
	env, venue, firstEntry := basketTwoLegBoundEnv(t)
	mock := env.attachNotifier()
	var fail atomic.Bool
	var fetches atomic.Int64
	env.bc.lots = basketColdLotCache(t, &fail, &fetches)
	btcQty := env.signedQty("hl-bsk-btc", "BTC")
	ethQty := env.signedQty("hl-bsk-eth", "ETH")
	at := firstEntry.Add(8*time.Hour + time.Second)
	env.bc.stepExpiry(basketCycleInputs{Now: at, Snapshot: basketTestSnapshot(at, 60, basketTestMarks(), 1)})
	if fetches.Load() == 0 {
		t.Fatal("the early pass must load the venue lot sizes before it sizes a tranche")
	}
	subs := env.expirySubmits(venue)
	if len(subs) == 0 {
		t.Fatal("the early pass sent no legal partial tranche on a cold lot cache")
	}
	full := map[string]float64{"BTC": math.Abs(btcQty), "ETH": math.Abs(ethQty)}
	if q := subs[0].RequestedQty; !(q > 0) || q >= full[subs[0].Symbol] {
		t.Fatalf("the first expiry tranche %+v must be a partial close under the $400 net bound", subs[0])
	}
	if n := countDMs(mock, "blocked by the basket bounds"); n != 0 {
		t.Fatalf("a cold lot cache produced %d false blocked-bound alerts", n)
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		if l, _, _ := env.lifecycleRow(id); l.alerted("expiry-blocked") {
			t.Fatalf("%s spent its expiry-blocked alert key on a cold lot cache: %+v", id, l)
		}
	}
}

func TestBasketExpiryEarlyPassHoldsWithoutABoundAlertWhenLotSizesAreUnavailable(t *testing.T) {
	env, venue, firstEntry := basketTwoLegBoundEnv(t)
	mock := env.attachNotifier()
	var fail atomic.Bool
	var fetches atomic.Int64
	fail.Store(true)
	cache := basketColdLotCache(t, &fail, &fetches)
	env.bc.lots = cache
	at := firstEntry.Add(8*time.Hour + time.Second)
	env.bc.stepExpiry(basketCycleInputs{Now: at, Snapshot: basketTestSnapshot(at, 60, basketTestMarks(), 1)})
	if fetches.Load() == 0 {
		t.Fatal("the early pass must try to load the venue lot sizes")
	}
	if n := len(env.expirySubmits(venue)); n != 0 {
		t.Fatalf("no partial tranche can be sized without lot sizes, yet %d expiry orders were sent", n)
	}
	if n := countDMs(mock, "blocked by the basket bounds"); n != 0 {
		t.Fatalf("a lot metadata outage produced %d blocked-bound alerts", n)
	}
	if n := countDMs(mock, "lot size unavailable"); n != 1 {
		t.Fatalf("lot outage alerts %d, want exactly one", n)
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		l, _, _ := env.lifecycleRow(id)
		if !l.active() || l.alerted("expiry-blocked") || l.RetryAtMs != at.Add(basketExpiryRetryFloor).UnixMilli() {
			t.Fatalf("%s must keep the obligation and a retry without the bound alert key: %+v", id, l)
		}
	}
	env.bc.stepExpiry(basketCycleInputs{Now: at.Add(61 * time.Second), Snapshot: basketTestSnapshot(at.Add(61*time.Second), 60, basketTestMarks(), 1)})
	if n := countDMs(mock, "lot size unavailable"); n != 1 {
		t.Fatalf("the retained lot alert key must suppress a repeat, got %d", n)
	}
	fail.Store(false)
	recovered := at.Add(time.Hour)
	wall := time.Now().Add(time.Hour)
	cache.clock = func() time.Time { return wall }
	for i := 0; i < 10 && !(env.signedQty("hl-bsk-btc", "BTC") == 0 && env.signedQty("hl-bsk-eth", "ETH") == 0); i++ {
		now := recovered.Add(time.Duration(i) * 2 * time.Minute)
		env.bc.stepExpiry(basketCycleInputs{Now: now, Snapshot: basketTestSnapshot(now, 60, basketTestMarks(), 1)})
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-eth"} {
		if q := env.signedQty(id, basketMemberCoinByID(id)); q != 0 {
			t.Fatalf("%s still holds %.10g after the lot sizes recovered", id, q)
		}
	}
	if n := countDMs(mock, "blocked by the basket bounds"); n != 0 {
		t.Fatalf("the recovered close produced %d blocked-bound alerts", n)
	}
}
