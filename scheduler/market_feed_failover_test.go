package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedFeedFailover(t *testing.T) {
	now := time.Now().UTC()
	minuteMs := int64(60_000)
	btc := feedKeyFor("BTC", "1m")
	eth := feedKeyFor("ETH", "1m")
	var candleCalls atomic.Int64
	origCandles := fetchHyperliquidCandleSnapshotFn
	fetchHyperliquidCandleSnapshotFn = func(ctx context.Context, coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		if err := feedBudgetAcquire(ctx, "candleSnapshot"); err != nil {
			return nil, err
		}
		candleCalls.Add(1)
		lastOpen := time.Now().UTC().UnixMilli() / minuteMs * minuteMs
		base := 100.0
		if coin == "ETH" {
			base = 10.0
		}
		out := make([]hlCandleRaw, 0, 260)
		for i := 259; i >= 0; i-- {
			open := lastOpen - int64(i)*minuteMs
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + minuteMs - 1, HasClose: true,
				Open: base, High: base + 2, Low: base - 1, Close: base + 1, Volume: 5})
		}
		return out, nil
	}
	origMids := fetchHyperliquidMidsCtxFn
	fetchHyperliquidMidsCtxFn = func(ctx context.Context, coins []string) (map[string]float64, error) {
		if err := feedBudgetAcquire(ctx, "allMids"); err != nil {
			return nil, err
		}
		return map[string]float64{"BTC": 101, "ETH": 11}, nil
	}
	t.Cleanup(func() {
		fetchHyperliquidCandleSnapshotFn = origCandles
		fetchHyperliquidMidsCtxFn = origMids
	})

	consumerA := feedRequirements{
		Keys:     map[marketFeedKey]int{btc: 200},
		MidCoins: []string{"BTC"},
		Funding:  map[string]feedFundingNeed{},
		Strategies: map[string]feedStrategyRequirement{
			"live-btc": {ID: "live-btc", Signal: btc, SignalLookback: 200, Coin: "BTC"},
		},
	}
	consumerA.finalize()
	consumerB := feedRequirements{
		Keys:     map[marketFeedKey]int{btc: 200, eth: 150},
		MidCoins: []string{"BTC", "ETH"},
		Funding:  map[string]feedFundingNeed{},
		Strategies: map[string]feedStrategyRequirement{
			"paper-btc": {ID: "paper-btc", Signal: btc, SignalLookback: 200, Coin: "BTC"},
			"paper-eth": {ID: "paper-eth", Signal: eth, SignalLookback: 150, Coin: "ETH"},
		},
	}
	consumerB.finalize()
	union := unionFeedRequirements([]feedConsumer{
		{Loaded: true, Req: consumerA, Cadences: []int{60}},
		{Loaded: true, Req: consumerB, Cadences: []int{60}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, err := os.MkdirTemp("", "gtf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	var alertMu sync.Mutex
	var feedAlerts []string
	newFeed := func(source string, ledger *feedRequestLedger) (*marketFeedOwner, *feedSealer) {
		owner := newMarketFeedOwner(nil, t.Logf)
		lctx := withFeedLedger(ctx, ledger)
		if ledger != nil {
			ledger.openStartup()
		}
		select {
		case <-owner.ApplyGeneration(lctx, union):
		case <-time.After(5 * time.Second):
			t.Fatalf("%s generation did not publish", source)
		}
		owner.IngestMids(map[string]float64{"BTC": 101, "ETH": 11}, time.Now().UTC(), "ws")
		sealer := newFeedSealer(owner, source, "inst-"+source, nil, t.Logf)
		sealer.settle = 0
		sealer.prepare = time.Hour
		sealer.ledger = ledger
		sealer.alertHook = func(msg string) {
			alertMu.Lock()
			feedAlerts = append(feedAlerts, msg)
			alertMu.Unlock()
		}
		if source == feedSourceREST {
			rest := newFeedRESTSource(owner, t.Logf)
			rest.backoff = 10 * time.Millisecond
			sealer.prepareFn = rest.prepare
		} else {
			sealer.prepareFn = websocketFeedPrepare(owner)
		}
		sealer.setGeneration(union, []int{60}, now.Add(-10*time.Minute))
		sealer.startServing(now.Add(-10 * time.Minute))
		return owner, sealer
	}
	listen := func(name string, sealer *feedSealer) *feedUnixServer {
		srv, err := listenFeedSocket(filepath.Join(dir, name+".sock"), sealer, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		go srv.serve()
		return srv
	}

	backupLedger := newFeedRequestLedger(&feedBudgetConfig{PerMinute: 1000, Startup: 100}, true, nil)
	_, primary := newFeed(feedSourceWebsocket, nil)
	_, backup := newFeed(feedSourceREST, backupLedger)
	primarySrv := listen("p", primary)
	backupSrv := listen("b", backup)
	defer func() { primarySrv.close(); backupSrv.close() }()

	k := now.Unix() / 60 * 60
	ka, kb, kc, kd := k-180, k-120, k-60, k
	for _, key := range []int64{ka, kb, kc, kd} {
		backup.sealOne(ctx, key)
	}
	if h := backup.lookup(ka); h.Status != feedWireStatusUnavailable || !strings.Contains(h.Detail, "evicted") {
		t.Fatalf("evicted backup key %d: status %q detail %q, want unavailable after eviction", ka, h.Status, h.Detail)
	}
	retained := append([]byte(nil), backup.sealBytes(kd)...)
	backup.sealOne(ctx, kd)
	if !bytes.Equal(retained, backup.sealBytes(kd)) {
		t.Fatal("a second backup seal attempt replaced immutable bytes")
	}

	client := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{
		{Name: "primary", Socket: filepath.Join(dir, "p.sock")},
		{Name: "backup", Socket: filepath.Join(dir, "b.sock")},
	})
	client.settle, client.prepare = 0, time.Hour
	reqsA := cycleRequirementsForDue([]StrategyConfig{{ID: "live-btc"}}, consumerA)
	hasAlert := func(r sharedFeedFetchReport, marker string) bool {
		for _, a := range r.Alerts {
			if strings.Contains(a, marker) {
				return true
			}
		}
		return false
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		primary.sealOne(ctx, kb)
	}()
	snap, rep := client.Fetch(ctx, kb, reqsA)
	if rep.Status != feedFetchSealed || rep.Endpoint != "primary" || rep.WaitedMs < 200 {
		t.Fatalf("pending within the bound: %+v, want the primary after waiting", rep)
	}
	if snap == nil || len(rep.Alerts) != 0 {
		t.Fatalf("pending within the bound raised alerts %v", rep.Alerts)
	}

	client.prepare = time.Now().Add(4500*time.Millisecond).Sub(time.Unix(kc, 0)) - client.grace - feedClientSlack
	started := time.Now()
	_, rep = client.Fetch(ctx, kc, reqsA)
	if rep.Status != feedFetchSealed || rep.Endpoint != "backup" || rep.Source != feedSourceREST {
		t.Fatalf("hung primary preparation: %+v, want the backup", rep)
	}
	if waited := time.Since(started); waited > 4500*time.Millisecond || !hasAlert(rep, "FAILOVER") {
		t.Fatalf("hung primary: waited %s, alerts %v; want backup service before the give-up with one FAILOVER alert", waited, rep.Alerts)
	}
	client.prepare = time.Hour

	primary.sealOne(ctx, kd)
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Endpoint != "primary" || !hasAlert(rep, "PRIMARY RESTORED") {
		t.Fatalf("primary return: %+v, want the primary with PRIMARY RESTORED", rep)
	}
	client.prepare = time.Now().Add(time.Second).Sub(time.Unix(kd, 0)) - client.grace - feedClientSlack
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Endpoint != "primary" || len(rep.Alerts) != 0 {
		t.Fatalf("fetch started after the primary's reserve bound: %+v, want the sealed primary with no alert", rep)
	}
	client.prepare = time.Hour

	primarySrv.close()
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Endpoint != "backup" || !hasAlert(rep, "FAILOVER") || hasAlert(rep, "OUTAGE") {
		t.Fatalf("primary loss: %+v, want the backup with one FAILOVER alert", rep)
	}
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Endpoint != "backup" || len(rep.Alerts) != 0 {
		t.Fatalf("second backup key: %+v, want no repeated alert", rep)
	}

	backupSrv.close()
	before := candleCalls.Load()
	snap, rep = client.Fetch(ctx, kd, reqsA)
	if snap == nil || rep.Status != feedFetchDegraded || !hasAlert(rep, "OUTAGE") {
		t.Fatalf("both endpoints down: snapshot %v report %+v, want a degraded snapshot and one OUTAGE alert", snap != nil, rep)
	}
	if _, ok := snap.frameFor(btc, 200); ok || candleCalls.Load() != before {
		t.Fatal("an outage produced a frame or a candle request on the consumer path")
	}
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Status != feedFetchDegraded || hasAlert(rep, "OUTAGE") {
		t.Fatalf("second outage key: %+v, want degraded with no repeated alert", rep)
	}

	backupSrv = listen("b", backup)
	_, rep = client.Fetch(ctx, kd, reqsA)
	if rep.Endpoint != "backup" || !hasAlert(rep, "RECOVERED") || rep.Hash != feedSealHash(retained) {
		t.Fatalf("backup return: %+v, want the retained backup seal with RECOVERED", rep)
	}

	clock := time.Now().UTC()
	var clockMu sync.Mutex
	tightLedger := newFeedRequestLedger(&feedBudgetConfig{PerMinute: 3, Startup: 2}, true, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	})
	_, tight := newFeed(feedSourceREST, tightLedger)
	tctx := withFeedLedger(ctx, tightLedger)
	tight.sealOne(tctx, kb)
	if s := tight.lookup(kb); s.Status != feedWireStatusSealed {
		t.Fatalf("first budgeted seal: %+v", s)
	}
	alertMu.Lock()
	feedAlerts = nil
	alertMu.Unlock()
	tight.sealOne(tctx, kc)
	doc, err := decodeFeedSeal(tight.sealBytes(kc), kc)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range doc.Keys {
		if key.Readiness.Ready || key.Readiness.Status != string(feedStatusBudget) {
			t.Fatalf("key %s|%s sealed ready=%v status %q after its refresh was refused, want budget_exhausted", key.Symbol, key.Timeframe, key.Readiness.Ready, key.Readiness.Status)
		}
	}
	clockMu.Lock()
	clock = clock.Add(61 * time.Second)
	clockMu.Unlock()
	tight.sealOne(tctx, kd)
	doc, err = decodeFeedSeal(tight.sealBytes(kd), kd)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range doc.Keys {
		if !key.Readiness.Ready {
			t.Fatalf("key %s|%s not ready after the window reopened: %+v", key.Symbol, key.Timeframe, key.Readiness)
		}
	}
	alertMu.Lock()
	got := append([]string(nil), feedAlerts...)
	alertMu.Unlock()
	if len(got) != 2 || !strings.Contains(got[0], "BUDGET EXHAUSTED") || !strings.Contains(got[1], "BUDGET RECOVERED") {
		t.Fatalf("budget alerts %q, want one EXHAUSTED and one RECOVERED", got)
	}
}
