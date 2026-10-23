package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quoteReplayHarnessLevels(mid int64, down bool) []map[string]any {
	out := make([]map[string]any, 0, feedBookMaxLevels)
	for i := int64(0); i < feedBookMaxLevels; i++ {
		px := mid + 5 + i*10
		if down {
			px = mid - 5 - i*10
		}
		out = append(out, map[string]any{"px": fmt.Sprintf("%d", px), "sz": fmt.Sprintf("0.%d", 2+i%7), "n": 1 + int(i)%5})
	}
	return out
}

func quoteReplayHarnessMid(ms int64) int64 {
	return int64(math.Round(100_000 + 40*math.Sin(float64(ms/1000)/90) + float64((ms/1000)%7) - 3))
}

func TestQuoteReplayRecordingHarness(t *testing.T) {
	kinds := []string{feedObservationTradeFlow, feedObservationL2Book}
	if os.Getenv("GO_TRADER_QUOTE_REPLAY_DEPTH_ONLY") == "1" {
		kinds = []string{feedObservationL2Book}
	}
	dropAt := int64(0)
	hour := int64(3_600_000)
	B := (time.Now().UTC().UnixMilli()/hour)*hour - 2*hour
	if os.Getenv("GO_TRADER_QUOTE_REPLAY_TRADE_GAP") == "1" {
		dropAt = B + 6*60_000
	}
	var clockMs atomic.Int64
	clockMs.Store(B - 30_000)
	clock := func() time.Time { return time.UnixMilli(clockMs.Load()).UTC() }
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	coins := []string{"BTC"}
	recDir := filepath.Join(t.TempDir(), "recording")
	rec, err := newObservationRecorder(recDir, observationRecorderMeta{RunID: "quote-replay-harness", Coins: coins, Kinds: kinds,
		Segment: 5 * time.Minute, WSURL: fs.url(), StartedAt: clock()}, clock, t.Logf)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.bookTick = l2TestTickPeriod
	trades := len(kinds) == 2
	if trades {
		owner.trackTradeFlows(coins, observationRecorderWindow.Milliseconds())
	}
	owner.attachRecorder(rec)
	owner.trackBooks(coins, recorderBookNeed())
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go owner.Run(ctx)
	waitFor(t, "connected", owner.Connected)
	waitFor(t, "subscriptions", func() bool {
		subs := strings.Join(fs.subscriptions(), " ")
		return strings.Contains(subs, `"coin":"BTC","fast":false,"type":"l2Book"`) && (!trades || strings.Contains(subs, `"coin":"BTC","type":"trades"`))
	})
	acks := func(at int64) {
		clockMs.Store(at)
		if trades {
			fs.broadcast(t, map[string]any{"channel": "subscriptionResponse", "data": map[string]any{"method": "subscribe", "subscription": map[string]any{"type": "trades", "coin": "BTC"}}})
		}
		fs.broadcast(t, l2TestAck("BTC", nil))
		waitFor(t, "acknowledgements", func() bool {
			owner.feedMu.Lock()
			defer owner.feedMu.Unlock()
			if !owner.books[l2BookKeyFor("BTC")].Covered {
				return false
			}
			return !trades || owner.tradeFlows[tradeFlowKeyFor("BTC")].CoverFromMs != 0
		})
	}
	counts := func() (uint64, uint64) {
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		var tr uint64
		if trades {
			tr = owner.tradeFlows[tradeFlowKeyFor("BTC")].Stats.Frames
		}
		return tr, owner.books[l2BookKeyFor("BTC")].Stats.Frames
	}
	acks(B - 25_000)
	var tid uint64 = 7_000_000
	end := B + 14*60_000
	for recv := B - 20_000; recv <= end; recv += 1000 {
		if dropAt > 0 && recv == dropAt {
			sessions := fs.sessionCount()
			clockMs.Store(recv)
			fs.dropConnections()
			waitFor(t, "reconnect", func() bool { return fs.sessionCount() > sessions && owner.Connected() })
			acks(recv + 500)
			continue
		}
		clockMs.Store(recv)
		mid := quoteReplayHarnessMid(recv)
		tr0, bk0 := counts()
		fs.broadcast(t, map[string]any{"channel": "l2Book", "data": map[string]any{"coin": "BTC", "time": recv - 300,
			"levels": []any{quoteReplayHarnessLevels(mid, true), quoteReplayHarnessLevels(mid, false)}}})
		if trades {
			tid++
			side, px := "B", mid+5
			if (recv/1000)%2 == 0 {
				side, px = "A", mid-5
			}
			if (recv/1000)%7 == 0 {
				side, px = "A", mid-120
			}
			if (recv/1000)%7 == 3 {
				side, px = "B", mid+120
			}
			fs.broadcast(t, map[string]any{"channel": "trades", "data": []any{map[string]any{"coin": "BTC", "side": side, "px": fmt.Sprintf("%d", px),
				"sz": "0.5", "time": recv - 300, "hash": fmt.Sprintf("0x%064x", tid), "tid": tid, "users": []string{"0xbuyer", "0xseller"}}}})
		}
		waitFor(t, fmt.Sprintf("frames at %d", recv), func() bool {
			tr, bk := counts()
			return bk > bk0 && (!trades || tr > tr0)
		})
		if trades && recv%60_000 == 0 {
			owner.AdvanceTradeFlows(clock())
		}
	}
	clockMs.Store(end + 90_000)
	if trades {
		owner.AdvanceTradeFlows(clock())
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
	blob, err := os.ReadFile(filepath.Join(recDir, "run.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Kinds   []string          `json:"kinds"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(blob, &run); err != nil || run.Sources[feedObservationL2Book] != feedBookSource || len(run.Kinds) != len(kinds) {
		t.Fatalf("run manifest: %s (%v)", blob, err)
	}
	out := os.Getenv("GO_TRADER_QUOTE_REPLAY_HARNESS_OUT")
	if out == "" {
		return
	}
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
	meta, _ := json.Marshal(map[string]int64{"base_ms": B, "end_ms": end})
	if err := os.WriteFile(filepath.Clean(out)+"-harness.json", meta, 0o644); err != nil {
		t.Fatal(err)
	}
}
