//go:build pyintegration

package main

import (
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const tszIntervalMs = int64(60_000)

type tszVenue struct {
	mu      sync.Mutex
	mark    float64
	unknown []string
}

func (v *tszVenue) setMark(px float64) {
	v.mu.Lock()
	v.mark = px
	v.mu.Unlock()
}

func (v *tszVenue) markPx() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mark
}

func (v *tszVenue) markString() string {
	return strconv.FormatFloat(v.markPx(), 'f', 4, 64)
}

func (v *tszVenue) candle(openMs int64) map[string]any {
	c := v.markPx()
	return map[string]any{
		"t": openMs, "T": openMs + tszIntervalMs - 1, "s": "BTC", "i": "1m",
		"o": strconv.FormatFloat(c, 'f', 4, 64), "h": strconv.FormatFloat(c+0.3, 'f', 4, 64),
		"l": strconv.FormatFloat(c-0.3, 'f', 4, 64), "c": strconv.FormatFloat(c, 'f', 4, 64),
		"v": "10", "n": 5,
	}
}

func (v *tszVenue) serveInfo(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Type string `json:"type"`
		Req  struct {
			StartTime int64 `json:"startTime"`
			EndTime   int64 `json:"endTime"`
		} `json:"req"`
	}
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	universe := []map[string]any{{"name": "BTC", "szDecimals": 5, "maxLeverage": 50}}
	switch req.Type {
	case "candleSnapshot":
		last := time.Now().UnixMilli() / tszIntervalMs * tszIntervalMs
		first := last - 399*tszIntervalMs
		out := []map[string]any{}
		for open := first; open <= last; open += tszIntervalMs {
			if (req.Req.StartTime == 0 || open >= req.Req.StartTime) && (req.Req.EndTime == 0 || open <= req.Req.EndTime) {
				out = append(out, v.candle(open))
			}
		}
		json.NewEncoder(w).Encode(out)
	case "allMids":
		json.NewEncoder(w).Encode(map[string]string{"BTC": v.markString()})
	case "fundingHistory":
		w.Write([]byte("[]"))
	case "meta":
		json.NewEncoder(w).Encode(map[string]any{"universe": universe})
	case "metaAndAssetCtxs":
		mark := v.markString()
		ctx := map[string]any{"funding": "0", "markPx": mark, "oraclePx": mark, "midPx": mark, "openInterest": "1000", "premium": "0", "dayNtlVlm": "1000000", "prevDayPx": mark}
		json.NewEncoder(w).Encode([]any{map[string]any{"universe": universe}, []any{ctx}})
	default:
		v.mu.Lock()
		v.unknown = append(v.unknown, string(body))
		v.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"unsupported by the local venue substitute"}`))
	}
}

func (v *tszVenue) serveSocket(stop <-chan struct{}) http.HandlerFunc {
	upgrader := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var writeMu sync.Mutex
		write := func(msg any) {
			writeMu.Lock()
			defer writeMu.Unlock()
			conn.WriteJSON(msg)
		}
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-stop:
					conn.Close()
					return
				case <-ticker.C:
					write(map[string]any{"channel": "allMids", "data": map[string]any{"mids": map[string]string{"BTC": v.markString()}}})
					write(map[string]any{"channel": "candle", "data": v.candle(time.Now().UnixMilli() / tszIntervalMs * tszIntervalMs)})
				}
			}
		}()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) != nil {
				continue
			}
			switch msg["method"] {
			case "ping":
				write(map[string]any{"channel": "pong"})
			case "subscribe":
				write(map[string]any{"channel": "subscriptionResponse", "data": msg})
			}
		}
	}
}

func tszLocalServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	return srv
}

func tszWriteConfig(t *testing.T, path, dir string, strategy map[string]any) {
	t.Helper()
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"),
		"market_feed": "websocket", "auto_update": "off",
		"strategies": []any{strategy},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tszReadPosition(t *testing.T, cfgPath, id string) *Position {
	t.Helper()
	cfg, err := LoadConfigReadOnly(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openToolStateStoreReadOnly(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	s := state.Strategies[id]
	if s == nil {
		t.Fatalf("strategy %s missing from the state store", id)
	}
	return s.Positions["BTC"]
}

func TestZeroTrailingStopPctReloadKeepsTheTrailingATRStop(t *testing.T) {
	t.Parallel()
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	const id = "hl-tsz-btc"

	venue := &tszVenue{mark: 100}
	info := tszLocalServer(t, http.HandlerFunc(venue.serveInfo))
	stopSocket := make(chan struct{})
	socket := tszLocalServer(t, venue.serveSocket(stopSocket))
	t.Cleanup(func() {
		close(stopSocket)
		socket.Close()
		info.Close()
	})

	strategy := map[string]any{
		"id": id, "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
		"args": []any{"hold", "BTC", "1m", "--mode=paper"}, "capital": 1000, "leverage": 1,
		"max_drawdown_pct": 90, "trailing_stop_atr_mult": 2,
	}
	cfgPath := filepath.Join(dir, "config.json")
	tszWriteConfig(t, cfgPath, dir, strategy)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("the trailing ATR config must load: %v", err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatal(err)
	}
	seed := NewAppState()
	ss := NewStrategyState(cfg.Strategies[0])
	ss.Positions["BTC"] = &Position{
		Symbol: "BTC", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 100, Leverage: 1,
		EntryATR: 2, OwnerStrategyID: id, TradePositionID: "tsz-pos-1",
		OpenedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
	}
	ss.Cash -= 100
	seed.Strategies[id] = ss
	for part, err := range store.SaveAll(seed) {
		if err != nil {
			t.Fatalf("seed %s: %v", partitionLabel(part), err)
		}
	}
	store.Close()

	d := &mrpDaemon{t: t, exited: make(chan struct{})}
	d.cmd = exec.Command(bin, "--config", cfgPath, "--status-port", strconv.Itoa(htfITFreePort(t)))
	d.cmd.Dir = root
	d.cmd.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"),
		"HTF_OFFLINE_FIXTURE="+filepath.Join(dir, "offline.json"),
		"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=",
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost",
		manualRegimeTestHLInfoEnv+"="+info.URL,
		manualRegimeTestHLWSEnv+"=ws"+strings.TrimPrefix(socket.URL, "http")+"/ws",
	)
	if err := os.WriteFile(filepath.Join(dir, "offline.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.cmd.Stdout, d.cmd.Stderr = d, d
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		d.cmd.Wait()
		close(d.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-d.exited:
		default:
			d.cmd.Process.Kill()
			<-d.exited
		}
	})

	tag := "[" + id + "] "
	armed := tag + "[INFO] Paper trailing SL trigger updated @ $96.0000 (high_water=$100.0000)"
	d.waitLog("the trailing ATR stop to arm on the seeded long", 240*time.Second, func(s string) bool { return strings.Contains(s, armed) })

	bad := map[string]any{}
	for k, v := range strategy {
		bad[k] = v
	}
	bad["trailing_stop_pct"] = 0
	tszWriteConfig(t, cfgPath, dir, bad)
	if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	refused := "strategy[" + id + "]: trailing_stop_pct=0 explicitly disables the stop and would hide trailing_stop_atr_mult=2"
	d.waitLog("the refused reload", 120*time.Second, func(s string) bool {
		return strings.Contains(s, "[reload] ERROR: reload failed; keeping previous config: config validation errors:") && strings.Contains(s, refused)
	})
	if strings.Contains(d.output(), "Config reload applied") {
		t.Fatalf("the refused reload applied a change:\n%s", d.output())
	}

	venue.setMark(110)
	advanced := tag + "[INFO] Paper trailing SL trigger updated @ $105.6000 (high_water=$110.0000)"
	d.waitLog("the trailing trigger to advance after the refused reload", 240*time.Second, func(s string) bool { return strings.Contains(s, advanced) })

	d.stop()
	if !strings.Contains(d.output(), "[shutdown] Complete.") {
		t.Fatalf("daemon did not shut down cleanly after the refused reload:\n%s", d.output())
	}
	venue.mu.Lock()
	unknown := append([]string(nil), venue.unknown...)
	venue.mu.Unlock()
	if len(unknown) > 0 {
		t.Fatalf("the daemon sent requests the local venue does not model: %v", unknown)
	}
	readback := filepath.Join(dir, "readback.json")
	tszWriteConfig(t, readback, dir, strategy)
	pos := tszReadPosition(t, readback, id)
	if pos == nil || pos.TradePositionID != "tsz-pos-1" || pos.Quantity != 1 || pos.Side != "long" {
		t.Fatalf("the seeded long must stay open as the same position, got %+v", pos)
	}
	if math.Abs(pos.StopLossTriggerPx-105.6) > 1e-9 || math.Abs(pos.StopLossHighWaterPx-110) > 1e-9 {
		t.Fatalf("the trailing ATR stop must keep trailing after the refused reload, got trigger=%v high_water=%v", pos.StopLossTriggerPx, pos.StopLossHighWaterPx)
	}
}
