package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSharedFeedParity(t *testing.T) {
	now := time.Now().UTC()
	hourMs := int64(3_600_000)
	lastOpen := now.UnixMilli() / hourMs * hourMs
	btc := feedKeyFor("BTC", "1h")
	eth := feedKeyFor("ETH", "1h")
	stubFeedCandleSnapshot(t, func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		base := 100.0
		if coin == "ETH" {
			base = 10.0
		}
		out := make([]hlCandleRaw, 0, 300)
		for i := 299; i >= 0; i-- {
			open := lastOpen - int64(i)*hourMs
			bar := testRawBar(open, base+float64(300-i)*0.25)
			bar.HasClose = i != 0
			out = append(out, bar)
		}
		return out, nil
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
		Keys:     map[marketFeedKey]int{btc: 200, eth: 120},
		MidCoins: []string{"BTC", "ETH"},
		Funding:  map[string]feedFundingNeed{},
		Strategies: map[string]feedStrategyRequirement{
			"paper-btc": {ID: "paper-btc", Signal: btc, SignalLookback: 200, Coin: "BTC"},
			"paper-eth": {ID: "paper-eth", Signal: eth, SignalLookback: 120, Coin: "ETH"},
		},
	}
	consumerB.finalize()
	union := unionFeedRequirements([]feedConsumer{{Loaded: true, Req: consumerA}, {Loaded: true, Req: consumerB}})

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

	dir, err := os.MkdirTemp("", "gtf")
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

	newClient := func() *sharedFeedClient {
		c := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: socket}})
		c.settle, c.prepare = 0, time.Hour
		return c
	}
	fetch := func(req feedRequirements, due []StrategyConfig) (*marketSnapshot, sharedFeedFetchReport) {
		snap, report := newClient().Fetch(ctx, key, cycleRequirementsForDue(due, req))
		if report.Status != feedFetchSealed {
			t.Fatalf("fetch key %d: %+v", key, report)
		}
		if len(report.Gaps) != 0 {
			t.Fatalf("coverage gaps: %v", report.Gaps)
		}
		return snap, report
	}
	snapA, repA := fetch(consumerA, []StrategyConfig{{ID: "live-btc"}})
	snapB, repB := fetch(consumerB, []StrategyConfig{{ID: "paper-btc"}, {ID: "paper-eth"}})
	if repA.Key != repB.Key || repA.Source != repB.Source || repA.Instance != repB.Instance || repA.Hash != repB.Hash || repA.Generation != repB.Generation {
		t.Fatalf("consumers disagree on the seal: A=%+v B=%+v", repA, repB)
	}
	if repA.Hash != feedSealHash(sealer.sealBytes(key)) {
		t.Fatalf("consumer hash %s is not the stored seal's hash", repA.Hash)
	}

	ctxA := &marketFeedContext{Enabled: true, Requirements: consumerA, Snapshot: snapA}
	ctxB := &marketFeedContext{Enabled: true, Requirements: consumerB, Snapshot: snapB}
	singleA, err := ctxA.singleCheckPayload(StrategyConfig{ID: "live-btc"})
	if err != nil {
		t.Fatal(err)
	}
	singleB, err := ctxB.singleCheckPayload(StrategyConfig{ID: "paper-btc"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(singleA, singleB) {
		t.Fatalf("equal frame specs produced different single-check payloads:\nA=%s\nB=%s", singleA, singleB)
	}
	batchKey := hlBatchKey{Symbol: "BTC", Timeframe: "1h", OhlcvLimit: 200}
	batchA, err := ctxA.batchPayload(batchKey, []StrategyConfig{{ID: "live-btc"}})
	if err != nil {
		t.Fatal(err)
	}
	batchB, err := ctxB.batchPayload(batchKey, []StrategyConfig{{ID: "paper-btc"}})
	if err != nil {
		t.Fatal(err)
	}
	blobA, _ := json.Marshal(batchA)
	blobB, _ := json.Marshal(batchB)
	if !bytes.Equal(blobA, blobB) {
		t.Fatalf("equal frame specs produced different batch payloads")
	}
	if frame := batchA.Frames[btc.PayloadID()]; len(frame.Rows) != 200 || !frame.Ready {
		t.Fatalf("batch frame carries %d rows (ready=%v), want 200 ready rows", len(frame.Rows), frame.Ready)
	}
	regime := regimeBundleRequest{Key: regimeBundleKey{Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h"}, OhlcvLimit: 100}
	regimeB, err := ctxB.regimeBundlePayload(regime)
	if err != nil {
		t.Fatal(err)
	}
	ctxA2 := &marketFeedContext{Enabled: true, Requirements: consumerB, Snapshot: snapA}
	regimeA, err := ctxA2.regimeBundlePayload(regime)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(regimeA, regimeB) {
		t.Fatalf("one seal produced different regime payloads for equal specs")
	}

	later := key + 60
	h := sealer.lookup(later)
	if h.Status != feedWireStatusPending {
		t.Fatalf("future key %d status %q, want pending", later, h.Status)
	}
	if h := sealer.lookup(key - 60*60); h.Status != feedWireStatusUnavailable {
		t.Fatalf("key before the first deadline is %q, want unavailable", h.Status)
	}
	before := append([]byte(nil), sealer.sealBytes(key)...)
	sealer.sealOne(ctx, key)
	if !bytes.Equal(before, sealer.sealBytes(key)) {
		t.Fatal("a second seal attempt replaced immutable bytes")
	}
}

func TestSharedFeedParityKeyFill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	btc := feedKeyFor("BTC", "1h")
	eth := feedKeyFor("ETH", "1h")
	h := newKeyFillHarness(t, ctx, 3_600_000, nil, map[string]bool{"ETH": true})
	wide := keyFillConsumer(map[marketFeedKey]int{btc: 200, eth: 150})
	narrow := keyFillConsumer(map[marketFeedKey]int{btc: 200, eth: 120})
	narrowPaper := keyFillConsumer(map[marketFeedKey]int{eth: 120})
	all := []feedConsumer{
		{Loaded: true, Req: wide, Cadences: []int{60}},
		{Loaded: true, Req: narrow, Cadences: []int{60}},
		{Loaded: true, Req: narrowPaper, Cadences: []int{60}},
	}
	primary := h.feed(feedSourceWebsocket, "parity-primary", nil, all...)
	backup := h.feed(feedSourceREST, "parity-backup", newFeedRequestLedger(&feedBudgetConfig{PerMinute: 5000, Startup: 500}, true, nil), all...)
	pSock, bSock := h.listen("p", primary), h.listen("b", backup)
	key := time.Now().UTC().Unix() / 60 * 60
	h.seal(primary, key)
	h.seal(backup, key)

	fetch := func(req feedRequirements) (*marketSnapshot, sharedFeedFetchReport) {
		c, _ := keyFillClient(pSock, bSock)
		snap, rep := c.Fetch(ctx, key, keyFillCycle(req))
		if rep.Status != feedFetchSealed || rep.Endpoint != "primary" || len(rep.Gaps) != 0 {
			t.Fatalf("fetch key %d: %+v", key, rep)
		}
		if !reflect.DeepEqual(rep.Filled, []string{"ETH|1h"}) || snap.keys[eth].FilledFrom != "backup" {
			t.Fatalf("filled %v, want ETH|1h from the backup", rep.Filled)
		}
		return snap, rep
	}
	snapWide, repWide := fetch(wide)
	snapNarrow, repNarrow := fetch(narrow)
	snapPaper, repPaper := fetch(narrowPaper)
	for _, r := range []sharedFeedFetchReport{repNarrow, repPaper} {
		if r.Hash != repWide.Hash || r.BackupInstance != repWide.BackupInstance || r.BackupHash != repWide.BackupHash || r.BackupSource != repWide.BackupSource {
			t.Fatalf("consumers selected different seals: %+v vs %+v", r, repWide)
		}
	}
	if repWide.BackupHash != feedSealHash(backup.sealBytes(key)) {
		t.Fatalf("backup hash %s is not the stored backup seal's hash", repWide.BackupHash)
	}
	if !reflect.DeepEqual(snapWide.keys[eth], snapNarrow.keys[eth]) || !reflect.DeepEqual(snapWide.keys[eth], snapPaper.keys[eth]) {
		t.Fatal("consumers with different lookbacks selected different ETH entries")
	}

	ctxNarrow := &marketFeedContext{Enabled: true, Requirements: narrow, Snapshot: snapNarrow}
	ctxPaper := &marketFeedContext{Enabled: true, Requirements: narrowPaper, Snapshot: snapPaper}
	singleA, err := ctxNarrow.singleCheckPayload(StrategyConfig{ID: "s-ETH"})
	if err != nil {
		t.Fatal(err)
	}
	singleB, err := ctxPaper.singleCheckPayload(StrategyConfig{ID: "s-ETH"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(singleA, singleB) {
		t.Fatalf("equal lookbacks produced different single-check payloads:\nA=%s\nB=%s", singleA, singleB)
	}
	batchKey := hlBatchKey{Symbol: "ETH", Timeframe: "1h", OhlcvLimit: 120}
	batchA, err := ctxNarrow.batchPayload(batchKey, []StrategyConfig{{ID: "s-ETH"}})
	if err != nil {
		t.Fatal(err)
	}
	batchB, err := ctxPaper.batchPayload(batchKey, []StrategyConfig{{ID: "s-ETH"}})
	if err != nil {
		t.Fatal(err)
	}
	blobA, _ := json.Marshal(batchA)
	blobB, _ := json.Marshal(batchB)
	if !bytes.Equal(blobA, blobB) {
		t.Fatal("equal lookbacks produced different batch payloads")
	}
	if frame := batchA.Frames[eth.PayloadID()]; len(frame.Rows) != 120 || !frame.Ready {
		t.Fatalf("batch frame carries %d rows (ready=%v), want 120 ready rows", len(frame.Rows), frame.Ready)
	}
	regime := regimeBundleRequest{Key: regimeBundleKey{Platform: "hyperliquid", Symbol: "ETH", Timeframe: "1h"}, OhlcvLimit: 100}
	regimeA, err := ctxNarrow.regimeBundlePayload(regime)
	if err != nil {
		t.Fatal(err)
	}
	regimeB, err := ctxPaper.regimeBundlePayload(regime)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(regimeA, regimeB) {
		t.Fatal("equal lookbacks produced different regime payloads")
	}

	only := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{{Name: "primary", Socket: bSock}})
	only.settle, only.prepare = 0, time.Hour
	snapBackup, repBackup := only.Fetch(ctx, key, keyFillCycle(narrow))
	if repBackup.Hash != repWide.BackupHash {
		t.Fatalf("backup-only client read hash %s, the fill read %s", repBackup.Hash, repWide.BackupHash)
	}
	frameFill, ok := snapNarrow.frameWithTiming(eth, 120, true)
	frameBackup, ok2 := snapBackup.frameWithTiming(eth, 120, true)
	if !ok || !ok2 {
		t.Fatalf("ETH frame present: fill %v backup-only %v", ok, ok2)
	}
	fillBlob, _ := json.Marshal(frameFill)
	backupBlob, _ := json.Marshal(frameBackup)
	if !bytes.Equal(fillBlob, backupBlob) {
		t.Fatal("a fill and a whole-seal backup read produced different ETH frame bytes")
	}
}
