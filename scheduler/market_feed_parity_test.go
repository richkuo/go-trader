package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
