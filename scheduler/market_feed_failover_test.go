package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
	newFeed := func(source string, ledger *feedRequestLedger, u feedRequirements) (*marketFeedOwner, *feedSealer) {
		owner := newMarketFeedOwner(nil, t.Logf)
		lctx := withFeedLedger(ctx, ledger)
		if ledger != nil {
			ledger.openStartup()
		}
		select {
		case <-owner.ApplyGeneration(lctx, u):
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
		sealer.setGeneration(u, []int{60}, now.Add(-10*time.Minute))
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
	_, primary := newFeed(feedSourceWebsocket, nil, union)
	_, backup := newFeed(feedSourceREST, backupLedger, union)
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
	_, tight := newFeed(feedSourceREST, tightLedger, union)
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

	mixed := unionFeedRequirements([]feedConsumer{
		{Loaded: true, Req: consumerA, Cadences: []int{60}},
		{Loaded: true, Req: consumerB, Cadences: []int{120}},
	})
	due := k / 120 * 120
	if due == k {
		due -= 120
	}
	notDue := due - 60
	restOwner, restMixed := newFeed(feedSourceREST, newFeedRequestLedger(&feedBudgetConfig{PerMinute: 1000, Startup: 100}, true, nil), mixed)
	_, wsMixed := newFeed(feedSourceWebsocket, nil, mixed)
	sealedReadiness := func(sealer *feedSealer, key int64) map[string]feedSealReadiness {
		sealer.sealOne(ctx, key)
		doc, err := decodeFeedSeal(sealer.sealBytes(key), key)
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]feedSealReadiness, len(doc.Keys))
		for _, sk := range doc.Keys {
			out[sk.Symbol] = sk.Readiness
		}
		return out
	}
	r := sealedReadiness(restMixed, notDue)
	if r["ETH"].Ready || r["ETH"].Status != string(feedStatusNotDue) || !r["BTC"].Ready {
		t.Fatalf("REST seal at %d with ETH cadence 120: %+v, want ETH not_due and BTC ready", notDue, r)
	}
	if own, ok := restOwner.readinessFor(eth); !ok || !own.Ready || own.Status == feedStatusNotDue {
		t.Fatalf("owner readiness for ETH after a not_due seal: %+v, want it unchanged and ready", own)
	}
	if r = sealedReadiness(restMixed, due); !r["ETH"].Ready || !r["BTC"].Ready {
		t.Fatalf("REST seal at %d, a multiple of 120: %+v, want every key ready", due, r)
	}
	for sym, rd := range sealedReadiness(wsMixed, notDue) {
		if rd.Status == string(feedStatusNotDue) {
			t.Fatalf("websocket seal marked %s not_due: %+v", sym, rd)
		}
	}
}

func TestSharedFeedFailoverKeyFill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	btc := feedKeyFor("BTC", "1m")
	eth := feedKeyFor("ETH", "1m")
	sol := feedKeyFor("SOL", "1m")
	xrp := feedKeyFor("XRP", "1m")
	avax := feedKeyFor("AVAX", "1m")
	doge := feedKeyFor("DOGE", "1m")
	h := newKeyFillHarness(t, ctx, 60_000,
		map[string]bool{"XRP": true},
		map[string]bool{"ETH": true, "DOGE": true})
	primaryNeed := keyFillConsumer(map[marketFeedKey]int{btc: 200, eth: 150, xrp: 150, avax: 100, doge: 150})
	backupNeed := keyFillConsumer(map[marketFeedKey]int{btc: 200, eth: 150, sol: 150, xrp: 150, avax: 150, doge: 100})
	newLedger := func() *feedRequestLedger {
		return newFeedRequestLedger(&feedBudgetConfig{PerMinute: 5000, Startup: 500}, true, nil)
	}
	primary := h.feed(feedSourceWebsocket, "inst-primary", nil, feedConsumer{Loaded: true, Req: primaryNeed, Cadences: []int{60}})
	backup := h.feed(feedSourceREST, "inst-backup", newLedger(), feedConsumer{Loaded: true, Req: backupNeed, Cadences: []int{60}})
	healthy := h.feed(feedSourceWebsocket, "inst-healthy", newLedger(), feedConsumer{Loaded: true, Req: primaryNeed, Cadences: []int{60}})
	pSock, bSock, hSock := h.listen("p", primary), h.listen("b", backup), h.listen("h", healthy)

	k := time.Now().UTC().Unix() / 60 * 60
	k1, k2 := k-60, k
	for _, s := range []*feedSealer{primary, backup, healthy} {
		h.seal(s, k1, k2)
	}
	ring := func() map[string][]byte {
		out := map[string][]byte{}
		for name, s := range map[string]*feedSealer{"primary": primary, "backup": backup} {
			for _, key := range []int64{k1, k2} {
				out[fmt.Sprintf("%s/%d", name, key)] = append([]byte(nil), s.sealBytes(key)...)
			}
		}
		return out
	}
	before := ring()
	for name, b := range before {
		if len(b) == 0 {
			t.Fatalf("seal %s is empty", name)
		}
	}
	if doc, err := decodeFeedSeal(primary.sealBytes(k2), k2); err != nil {
		t.Fatal(err)
	} else {
		for _, sk := range doc.Keys {
			want := sk.Symbol != "ETH" && sk.Symbol != "XRP" && sk.Symbol != "DOGE"
			if sk.Readiness.Ready != want {
				t.Fatalf("primary %s sealed ready=%v status %q, want ready=%v", sk.Symbol, sk.Readiness.Ready, sk.Readiness.Status, want)
			}
		}
	}
	alertCount := func(r sharedFeedFetchReport, marker string) int {
		n := 0
		for _, a := range r.Alerts {
			if strings.Contains(a, marker) {
				n++
			}
		}
		return n
	}
	consumerOf := func(need map[marketFeedKey]int) (feedRequirements, cycleMarketRequirements) {
		r := keyFillConsumer(need)
		return r, keyFillCycle(r)
	}

	t.Run("fills_unready_key", func(t *testing.T) {
		c, dials := keyFillClient(pSock, bSock)
		req, cycle := consumerOf(map[marketFeedKey]int{btc: 200, eth: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if rep.Status != feedFetchSealed || rep.Endpoint != "primary" || rep.Hash != feedSealHash(before[fmt.Sprintf("primary/%d", k2)]) {
			t.Fatalf("report %+v, want the primary seal", rep)
		}
		if !reflect.DeepEqual(rep.Filled, []string{"ETH|1m"}) || rep.BackupHash != feedSealHash(before[fmt.Sprintf("backup/%d", k2)]) || rep.BackupInstance != "inst-backup" {
			t.Fatalf("filled %v backup %s/%s, want ETH|1m from the backup seal", rep.Filled, rep.BackupInstance, rep.BackupHash)
		}
		if len(rep.Unready) != 0 || len(rep.Gaps) != 0 || dials(bSock) != 1 {
			t.Fatalf("unready %v gaps %v backup dials %d, want none, none and 1", rep.Unready, rep.Gaps, dials(bSock))
		}
		if snap.keys[btc].FilledFrom != "" || snap.keys[eth].FilledFrom != "backup" || !snap.keys[eth].Readiness.Ready {
			t.Fatalf("BTC from %q, ETH from %q ready=%v; want BTC primary and ETH backup", snap.keys[btc].FilledFrom, snap.keys[eth].FilledFrom, snap.keys[eth].Readiness.Ready)
		}
		if rep.FillDetails[0].PrimaryStatus != string(feedStatusFailed) {
			t.Fatalf("fill details %+v, want the primary failed status", rep.FillDetails)
		}
		fctx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap}
		if _, err := fctx.singleCheckPayload(StrategyConfig{ID: "s-ETH"}); err != nil {
			t.Fatalf("ETH payload after the fill: %v", err)
		}
		only := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: pSock}})
		only.settle, only.prepare = 0, time.Hour
		osnap, _ := only.Fetch(ctx, k2, cycle)
		if _, err := (&marketFeedContext{Enabled: true, Requirements: req, Snapshot: osnap}).singleCheckPayload(StrategyConfig{ID: "s-ETH"}); err == nil {
			t.Fatal("ETH payload without a backup endpoint succeeded, want the primary hold")
		}
		line := sharedFeedAuditLine(rep, 2, []int{60})
		for _, want := range []string{"status=sealed endpoint=primary ", "backup_hash=" + rep.BackupHash + " ", "filled=ETH|1m ", "unready=none ", "backup_instance=inst-backup "} {
			if !strings.Contains(line, want) {
				t.Fatalf("audit line %q lacks %q", line, want)
			}
		}
		if alertCount(rep, "KEY FALLBACK**") != 1 || len(rep.Alerts) != 1 {
			t.Fatalf("alerts %v, want one KEY FALLBACK", rep.Alerts)
		}
		if !reflect.DeepEqual(ring(), before) {
			t.Fatal("a fill changed seal bytes in a feed ring")
		}
		for name, b := range ring() {
			if strings.HasPrefix(name, "backup/") && strings.HasSuffix(name, fmt.Sprint(k2)) && feedSealHash(b) != rep.BackupHash {
				t.Fatalf("backup ring hash %s differs from the reported %s", feedSealHash(b), rep.BackupHash)
			}
		}
	})

	t.Run("missing_primary_key_filled", func(t *testing.T) {
		c, _ := keyFillClient(pSock, bSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, sol: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if !reflect.DeepEqual(rep.Filled, []string{"SOL|1m"}) || len(rep.Gaps) != 0 || rep.FillDetails[0].PrimaryStatus != "missing" {
			t.Fatalf("filled %v gaps %v details %+v, want SOL filled from a missing primary key", rep.Filled, rep.Gaps, rep.FillDetails)
		}
		if _, ok := snap.frameFor(sol, 150); !ok || alertCount(rep, "COVERAGE GAP") != 0 {
			t.Fatalf("SOL frame missing or a coverage gap raised: %v", rep.Alerts)
		}
	})

	t.Run("no_request_when_ready", func(t *testing.T) {
		c, dials := keyFillClient(pSock, bSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200})
		_, rep := c.Fetch(ctx, k2, cycle)
		if dials(bSock) != 0 || rep.FillRequest != "none" || len(rep.Filled) != 0 || rep.BackupHash != "" {
			t.Fatalf("backup dials %d fill %q filled %v, want no backup request", dials(bSock), rep.FillRequest, rep.Filled)
		}
	})

	t.Run("unready_in_both", func(t *testing.T) {
		c, dials := keyFillClient(pSock, bSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, xrp: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if dials(bSock) != 1 || len(rep.Filled) != 0 || len(rep.Unready) != 1 || rep.Unready[0].Key != "XRP|1m" || rep.Unready[0].BackupStatus == "" {
			t.Fatalf("dials %d filled %v unready %+v, want XRP unready in both seals", dials(bSock), rep.Filled, rep.Unready)
		}
		if _, ok := snap.frameFor(xrp, 150); ok || alertCount(rep, "KEY FALLBACK") != 0 {
			t.Fatalf("XRP frame present or fallback alert %v", rep.Alerts)
		}
		if line := sharedFeedAuditLine(rep, 2, []int{60}); !strings.Contains(line, "unready=XRP|1m ") || !strings.Contains(line, "filled=none ") {
			t.Fatalf("audit line %q, want unready=XRP|1m and filled=none", line)
		}
		if st := c.status(); len(st.Fallbacks) != 0 {
			t.Fatalf("episodes %+v opened for a key ready in neither seal", st.Fallbacks)
		}
	})

	t.Run("short_primary_lookback_not_filled", func(t *testing.T) {
		c, dials := keyFillClient(pSock, bSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, avax: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if dials(bSock) != 0 || len(rep.Filled) != 0 || len(rep.Gaps) != 1 || !strings.Contains(rep.Gaps[0], "AVAX|1m lookback 100 is below the 150") {
			t.Fatalf("dials %d filled %v gaps %v, want no request and an AVAX coverage gap", dials(bSock), rep.Filled, rep.Gaps)
		}
		if !snap.keyFailed(avax) {
			t.Fatal("AVAX is not held after its coverage gap")
		}
	})

	t.Run("backup_lookback_short", func(t *testing.T) {
		c, _ := keyFillClient(pSock, bSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, doge: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if !reflect.DeepEqual(rep.Filled, []string{"DOGE|1m"}) || len(rep.Gaps) != 1 || !strings.Contains(rep.Gaps[0], "in the backup seal") {
			t.Fatalf("filled %v gaps %v, want DOGE filled then removed with a gap naming the backup", rep.Filled, rep.Gaps)
		}
		if !snap.keyFailed(doge) || alertCount(rep, "KEY FALLBACK") != 0 || len(c.status().Fallbacks) != 0 {
			t.Fatalf("DOGE held=%v alerts %v episodes %+v, want held with no fallback", snap.keyFailed(doge), rep.Alerts, c.status().Fallbacks)
		}
	})

	t.Run("backup_not_due", func(t *testing.T) {
		due := k / 120 * 120
		if due == k {
			due -= 120
		}
		notDue := due - 60
		mixed := h.feed(feedSourceREST, "inst-mixed", newLedger(),
			feedConsumer{Loaded: true, Req: keyFillConsumer(map[marketFeedKey]int{btc: 200}), Cadences: []int{60}},
			feedConsumer{Loaded: true, Req: keyFillConsumer(map[marketFeedKey]int{eth: 150}), Cadences: []int{120}})
		mSock := h.listen("m", mixed)
		h.seal(primary, notDue)
		h.seal(mixed, notDue)
		c, dials := keyFillClient(pSock, mSock)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, eth: 150})
		snap, rep := c.Fetch(ctx, notDue, cycle)
		if dials(mSock) != 1 || len(rep.Filled) != 0 || len(rep.Unready) != 1 || rep.Unready[0].BackupStatus != string(feedStatusNotDue) {
			t.Fatalf("dials %d filled %v unready %+v, want ETH held with the backup not_due", dials(mSock), rep.Filled, rep.Unready)
		}
		if !snap.keyFailed(eth) {
			t.Fatal("ETH is not held when the backup entry is not_due")
		}
	})

	t.Run("stalled_backup_socket", func(t *testing.T) {
		stall := h.stalled("s")
		c, dials := keyFillClient(pSock, stall)
		c.prepare = time.Now().Add(1500*time.Millisecond).Sub(time.Unix(k2, 0)) - c.grace - feedClientSlack
		giveUp := c.giveUpAt(k2)
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, eth: 150})
		started := time.Now()
		snap, rep := c.Fetch(ctx, k2, cycle)
		elapsed := time.Since(started)
		late := time.Since(giveUp)
		if dials(stall) != 1 || rep.Endpoint != "primary" || len(rep.Filled) != 0 || !snap.keyFailed(eth) {
			t.Fatalf("dials %d report %+v, want one abandoned fill request and ETH held", dials(stall), rep)
		}
		if elapsed > 3*time.Second || late < -100*time.Millisecond || late > time.Second {
			t.Fatalf("fill request ended after %s, %s from the give-up time; want it abandoned at the give-up time, well under the %s I/O timeout", elapsed, late, feedClientIOTimeout)
		}
		if !strings.Contains(rep.FillRequest, feedErrTransport) {
			t.Fatalf("fill request %q, want a transport error at the give-up time", rep.FillRequest)
		}
	})

	t.Run("no_request_at_give_up", func(t *testing.T) {
		c, dials := keyFillClient(pSock, bSock)
		var passed atomic.Bool
		inner := c.dial
		c.dial = func(ctx context.Context, path string) (net.Conn, error) {
			passed.Store(true)
			return inner(ctx, path)
		}
		giveUp := c.giveUpAt(k2)
		c.clock = func() time.Time {
			if passed.Load() {
				return giveUp.Add(time.Second)
			}
			return time.Now().UTC()
		}
		_, cycle := consumerOf(map[marketFeedKey]int{btc: 200, eth: 150})
		snap, rep := c.Fetch(ctx, k2, cycle)
		if dials(pSock) != 1 || dials(bSock) != 0 || rep.Endpoint != "primary" || !snap.keyFailed(eth) {
			t.Fatalf("primary dials %d backup dials %d report %+v, want no fill request after the give-up time", dials(pSock), dials(bSock), rep)
		}
		if !strings.Contains(rep.FillRequest, feedErrPendingLimit) || rep.Attempts[len(rep.Attempts)-1] != "backup(fill)="+rep.FillRequest {
			t.Fatalf("fill request %q attempts %v, want a recorded give-up refusal", rep.FillRequest, rep.Attempts)
		}
	})

	t.Run("episodes", func(t *testing.T) {
		c, _ := keyFillClient(pSock, bSock)
		both, bothCycle := consumerOf(map[marketFeedKey]int{btc: 200, eth: 150})
		_, btcCycle := consumerOf(map[marketFeedKey]int{btc: 200})
		_, rep := c.Fetch(ctx, k1, bothCycle)
		st := c.status()
		if alertCount(rep, "KEY FALLBACK**") != 1 || len(st.Fallbacks) != 1 || st.Fallbacks[0].Key != "ETH|1m" || st.Fallbacks[0].StartKey != k1 {
			t.Fatalf("first fill: alerts %v episodes %+v, want one start", rep.Alerts, st.Fallbacks)
		}
		_, rep = c.Fetch(ctx, k2, bothCycle)
		if len(rep.Alerts) != 0 || c.status().Fallbacks[0].Cycles != 2 || c.status().Fallbacks[0].LastKey != k2 {
			t.Fatalf("second fill: alerts %v episodes %+v, want no alert and two cycles", rep.Alerts, c.status().Fallbacks)
		}
		_, rep = c.Fetch(ctx, k2, btcCycle)
		if len(rep.Alerts) != 0 || len(c.status().Fallbacks) != 1 {
			t.Fatalf("cycle without ETH: alerts %v episodes %+v, want the episode kept", rep.Alerts, c.status().Fallbacks)
		}
		c.endpoints[0].Socket = hSock
		snap, rep := c.Fetch(ctx, k2, bothCycle)
		if alertCount(rep, "KEY FALLBACK ENDED**") != 1 || len(rep.Alerts) != 1 || len(c.status().Fallbacks) != 0 || snap.keys[eth].FilledFrom != "" || len(rep.Filled) != 0 {
			t.Fatalf("ready primary: alerts %v episodes %+v filled %v, want one end", rep.Alerts, c.status().Fallbacks, rep.Filled)
		}
		c.endpoints[0].Socket = pSock
		_, rep = c.Fetch(ctx, k2, bothCycle)
		if alertCount(rep, "KEY FALLBACK**") != 1 || len(c.status().Fallbacks) != 1 {
			t.Fatalf("second episode: alerts %v episodes %+v", rep.Alerts, c.status().Fallbacks)
		}
		if st := c.status(); st.Last == nil || !reflect.DeepEqual(st.Last.Filled, []string{"ETH|1m"}) {
			t.Fatalf("status last report %+v, want the filled set", st.Last)
		}
		if closed := c.closeFallbackEpisodes(both); len(closed) != 0 {
			t.Fatalf("reload with ETH closed %v", closed)
		}
		btcOnly := keyFillConsumer(map[marketFeedKey]int{btc: 200})
		if closed := c.closeFallbackEpisodes(btcOnly); !reflect.DeepEqual(closed, []string{"ETH|1m"}) || len(c.status().Fallbacks) != 0 {
			t.Fatalf("reload without ETH closed %v, episodes %+v; want ETH closed", closed, c.status().Fallbacks)
		}
		for _, a := range append([]string(nil), rep.Alerts...) {
			for _, marker := range []string{"COVERAGE GAP", "FAILOVER", "RECOVERED", "OUTAGE"} {
				if strings.Contains(a, marker) {
					t.Fatalf("fallback alert %q holds the %s marker", a, marker)
				}
			}
		}
	})
}
