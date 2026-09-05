package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type feedSocketServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	subs     []string
	sessions int
	conns    []*websocket.Conn
	ready    chan struct{}
}

func newFeedSocketServer(t *testing.T) *feedSocketServer {
	t.Helper()
	fs := &feedSocketServer{ready: make(chan struct{}, 8)}
	upgrader := websocket.Upgrader{}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.sessions++
		fs.conns = append(fs.conns, conn)
		fs.mu.Unlock()
		select {
		case fs.ready <- struct{}{}:
		default:
		}
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			if msg["method"] == "ping" {
				conn.WriteJSON(map[string]any{"channel": "pong"})
				continue
			}
			if msg["method"] == "subscribe" {
				blob, _ := json.Marshal(msg["subscription"])
				fs.mu.Lock()
				fs.subs = append(fs.subs, string(blob))
				fs.mu.Unlock()
			}
		}
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *feedSocketServer) url() string {
	return "ws" + strings.TrimPrefix(fs.server.URL, "http") + "/ws"
}

func (fs *feedSocketServer) subscriptions() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.subs...)
}

func (fs *feedSocketServer) sessionCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.sessions
}

func (fs *feedSocketServer) broadcast(t *testing.T, payload any) {
	t.Helper()
	fs.mu.Lock()
	conns := append([]*websocket.Conn(nil), fs.conns...)
	fs.mu.Unlock()
	for _, c := range conns {
		if err := c.WriteJSON(payload); err != nil {
			t.Fatalf("broadcast: %v", err)
		}
	}
}

func (fs *feedSocketServer) dropConnections() {
	fs.mu.Lock()
	conns := append([]*websocket.Conn(nil), fs.conns...)
	fs.conns = nil
	fs.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stubFeedCandleSnapshot(t *testing.T, fn func(coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error)) {
	t.Helper()
	orig := fetchHyperliquidCandleSnapshotFn
	fetchHyperliquidCandleSnapshotFn = func(_ context.Context, coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		return fn(coin, interval, startMs, endMs)
	}
	t.Cleanup(func() { fetchHyperliquidCandleSnapshotFn = orig })
}

func feedTestRequirements(key marketFeedKey, required int, coins ...string) feedRequirements {
	req := feedRequirements{Keys: map[marketFeedKey]int{key: required}, MidCoins: coins}
	req.Funding = map[string]feedFundingNeed{}
	req.Strategies = map[string]feedStrategyRequirement{}
	req.finalize()
	return req
}

func TestMarketFeedSocketContract(t *testing.T) {
	base := int64(1_700_000_000_000)
	key := testFeedKey()

	t.Run("subscribes to every key and to allMids, then ingests both channels", func(t *testing.T) {
		fs := newFeedSocketServer(t)
		hlFeedWebsocketURLOverride = fs.url()
		defer func() { hlFeedWebsocketURLOverride = "" }()
		stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
			return testRawSeries(base, 40), nil
		})

		owner := newMarketFeedOwner(nil, nil)
		<-owner.ApplyGeneration(context.Background(), feedTestRequirements(key, 40, "BTC"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go owner.Run(ctx)

		waitFor(t, "subscriptions", func() bool { return len(fs.subscriptions()) >= 2 })
		subs := strings.Join(fs.subscriptions(), " ")
		if !strings.Contains(subs, `"type":"allMids"`) {
			t.Fatalf("allMids subscription missing: %v", subs)
		}
		if !strings.Contains(subs, `"coin":"BTC"`) || !strings.Contains(subs, `"interval":"1h"`) {
			t.Fatalf("candle subscription missing: %v", subs)
		}

		newOpen := base + 40*testFeedIntervalMs
		fs.broadcast(t, map[string]any{
			"channel": "candle",
			"data": map[string]any{
				"t": newOpen, "T": newOpen + testFeedIntervalMs - 1, "s": "BTC", "i": "1h",
				"o": "140", "h": "142", "l": "137", "c": "141", "v": "9",
			},
		})
		fs.broadcast(t, map[string]any{
			"channel": "allMids",
			"data":    map[string]any{"mids": map[string]string{"BTC": "141.5", "ETH": "3000"}},
		})

		waitFor(t, "candle ingest", func() bool {
			owner.feedMu.Lock()
			defer owner.feedMu.Unlock()
			st := owner.keys[key]
			return st != nil && len(st.Bars) == 41
		})
		waitFor(t, "mid ingest", func() bool {
			owner.feedMu.Lock()
			defer owner.feedMu.Unlock()
			return owner.mids["BTC"].Px == 141.5
		})
		owner.feedMu.Lock()
		_, trackedETH := owner.mids["ETH"]
		owner.feedMu.Unlock()
		if trackedETH {
			t.Fatalf("a mid outside the requirement set must not be stored")
		}
	})

	t.Run("a malformed candle message never reaches the ring", func(t *testing.T) {
		st := newFeedKeyState(key, testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		owner := newMarketFeedOwner(nil, nil)
		owner.keys[key] = st
		owner.handleSocketMessage([]byte(`{"channel":"candle","data":{"s":"BTC","i":"1h","o":"1"}}`))
		owner.handleSocketMessage([]byte(`not json`))
		owner.handleSocketMessage([]byte(`{"channel":"allMids","data":{"mids":{"BTC":"not-a-number"}}}`))
		if len(st.Bars) != 0 {
			t.Fatalf("a malformed candle reached the ring: %+v", st.Bars)
		}
	})

	t.Run("a candle for an untracked key is dropped", func(t *testing.T) {
		owner := newMarketFeedOwner(nil, nil)
		owner.handleSocketMessage([]byte(`{"channel":"candle","data":{"t":1,"T":2,"s":"DOGE","i":"1h","o":"1","h":"2","l":"0.5","c":"1.5","v":"1"}}`))
		owner.feedMu.Lock()
		defer owner.feedMu.Unlock()
		if len(owner.keys) != 0 {
			t.Fatalf("an untracked key must not be created by a socket message")
		}
	})

	t.Run("reconnect repairs the key before it serves again", func(t *testing.T) {
		fs := newFeedSocketServer(t)
		hlFeedWebsocketURLOverride = fs.url()
		defer func() { hlFeedWebsocketURLOverride = "" }()
		var repairCalls int
		var mu sync.Mutex
		stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
			mu.Lock()
			repairCalls++
			mu.Unlock()
			return testRawSeries(base, 40), nil
		})

		owner := newMarketFeedOwner(nil, nil)
		<-owner.ApplyGeneration(context.Background(), feedTestRequirements(key, 40, "BTC"))
		mu.Lock()
		afterBootstrap := repairCalls
		mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go owner.Run(ctx)
		waitFor(t, "first session", func() bool { return fs.sessionCount() >= 1 })
		waitFor(t, "connected", owner.Connected)

		fs.dropConnections()
		waitFor(t, "second session", func() bool { return fs.sessionCount() >= 2 })
		waitFor(t, "repair call", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return repairCalls > afterBootstrap
		})
		if owner.Metrics().RepairCalls == 0 {
			t.Fatalf("a reconnect must count a repair call: %+v", owner.Metrics())
		}
		if owner.Metrics().SteadyCandleCalls != 0 {
			t.Fatalf("steady-state candle REST calls must stay 0: %+v", owner.Metrics())
		}
	})
}

func TestMarketFeedStopsOnShutdown(t *testing.T) {
	resetShutdownState(t)
	t.Cleanup(func() { resetShutdownState(t) })
	fs := newFeedSocketServer(t)
	hlFeedWebsocketURLOverride = fs.url()
	defer func() { hlFeedWebsocketURLOverride = "" }()
	stubFeedCandleSnapshot(t, func(string, string, int64, int64) ([]hlCandleRaw, error) {
		return testRawSeries(1_700_000_000_000, 40), nil
	})

	owner := newMarketFeedOwner(nil, nil)
	<-owner.ApplyGeneration(shutdownReadOnlyCtx, feedTestRequirements(testFeedKey(), 40, "BTC"))

	done := make(chan struct{})
	go func() {
		owner.Run(shutdownReadOnlyCtx)
		close(done)
	}()
	waitFor(t, "connected", owner.Connected)

	beginDrain()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the feed goroutine did not exit on the shutdown context")
	}
}
