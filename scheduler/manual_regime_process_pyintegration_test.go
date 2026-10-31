//go:build pyintegration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const mrpIntervalMs = int64(60_000)

type mrpVenue struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	bars    int
	unknown []string
	counts  map[string]int
}

var mrpCoins = []string{"BTC"}

func mrpBarClose(openMs int64) float64 {
	return 100 + float64((openMs/mrpIntervalMs)%500)*0.05
}

func mrpMids() map[string]string {
	out := map[string]string{}
	for _, coin := range mrpCoins {
		out[coin] = strconv.FormatFloat(mrpMark(), 'f', 4, 64)
	}
	return out
}

func mrpCandle(coin string, openMs int64) map[string]any {
	c := mrpBarClose(openMs)
	return map[string]any{
		"t": openMs, "T": openMs + mrpIntervalMs - 1, "s": coin, "i": "1m",
		"o": strconv.FormatFloat(c-0.02, 'f', 4, 64), "h": strconv.FormatFloat(c+0.3, 'f', 4, 64),
		"l": strconv.FormatFloat(c-0.3, 'f', 4, 64), "c": strconv.FormatFloat(c, 'f', 4, 64),
		"v": "10", "n": 5,
	}
}

func mrpMark() float64 {
	return mrpBarClose(time.Now().UnixMilli() / mrpIntervalMs * mrpIntervalMs)
}

func newMRPVenue(t *testing.T, bars int) *mrpVenue {
	t.Helper()
	v := &mrpVenue{t: t, bars: bars, counts: map[string]int{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	v.server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(v.serve)}}
	v.server.Start()
	t.Cleanup(v.server.Close)
	return v
}

func (v *mrpVenue) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Type string `json:"type"`
		Req  struct {
			Coin      string `json:"coin"`
			Interval  string `json:"interval"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		} `json:"req"`
	}
	_ = json.Unmarshal(body, &req)
	v.mu.Lock()
	v.counts[req.Type]++
	bars := v.bars
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	universe := []map[string]any{{"name": "BTC", "szDecimals": 5, "maxLeverage": 50}, {"name": "ETH", "szDecimals": 4, "maxLeverage": 50}}
	switch req.Type {
	case "candleSnapshot":
		last := time.Now().UnixMilli() / mrpIntervalMs * mrpIntervalMs
		first := last - int64(bars-1)*mrpIntervalMs
		var out []map[string]any
		for open := first; open <= last; open += mrpIntervalMs {
			if (req.Req.StartTime == 0 || open >= req.Req.StartTime) && (req.Req.EndTime == 0 || open <= req.Req.EndTime) {
				out = append(out, mrpCandle(req.Req.Coin, open))
			}
		}
		json.NewEncoder(w).Encode(out)
	case "allMids":
		json.NewEncoder(w).Encode(mrpMids())
	case "fundingHistory":
		w.Write([]byte("[]"))
	case "meta":
		json.NewEncoder(w).Encode(map[string]any{"universe": universe})
	case "metaAndAssetCtxs":
		mark := strconv.FormatFloat(mrpMark(), 'f', 4, 64)
		ctx := map[string]any{"funding": "0.0000125", "markPx": mark, "oraclePx": mark, "midPx": mark, "openInterest": "1000", "premium": "0", "dayNtlVlm": "1000000", "prevDayPx": mark}
		json.NewEncoder(w).Encode([]any{map[string]any{"universe": universe}, []any{ctx, ctx}})
	default:
		v.mu.Lock()
		v.unknown = append(v.unknown, string(body))
		v.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"unsupported by the local venue substitute"}`))
	}
}

func (v *mrpVenue) setBars(n int) {
	v.mu.Lock()
	v.bars = n
	v.mu.Unlock()
}

type mrpSocket struct {
	server *httptest.Server
	mu     sync.Mutex
	conns  []*websocket.Conn
	stop   chan struct{}
}

func newMRPSocket(t *testing.T) *mrpSocket {
	t.Helper()
	s := &mrpSocket{stop: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var writeMu sync.Mutex
		write := func(v any) {
			writeMu.Lock()
			defer writeMu.Unlock()
			conn.WriteJSON(v)
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-s.stop:
					return
				case <-ticker.C:
					now := time.Now().UnixMilli() / mrpIntervalMs * mrpIntervalMs
					write(map[string]any{"channel": "allMids", "data": map[string]any{"mids": mrpMids()}})
					for _, coin := range mrpCoins {
						write(map[string]any{"channel": "candle", "data": mrpCandle(coin, now)})
					}
				}
			}
		}()
		defer close(done)
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
				write(map[string]any{"channel": "pong"})
			}
			if msg["method"] == "subscribe" {
				write(map[string]any{"channel": "subscriptionResponse", "data": msg})
			}
		}
	})}}
	s.server.Start()
	t.Cleanup(func() {
		close(s.stop)
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.server.Close()
	})
	return s
}

func (s *mrpSocket) url() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + "/ws"
}

type mrpDaemon struct {
	t      *testing.T
	cmd    *exec.Cmd
	port   int
	logMu  sync.Mutex
	log    bytes.Buffer
	exited chan struct{}
}

func (d *mrpDaemon) output() string {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	return d.log.String()
}

func (d *mrpDaemon) Write(p []byte) (int, error) {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	return d.log.Write(p)
}

func (d *mrpDaemon) waitLog(what string, timeout time.Duration, cond func(string) bool) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond(d.output()) {
			return
		}
		select {
		case <-d.exited:
			d.t.Fatalf("daemon exited while waiting for %s:\n%s", what, d.output())
		case <-time.After(100 * time.Millisecond):
		}
	}
	d.t.Fatalf("timed out waiting for %s:\n%s", what, d.output())
}

func (d *mrpDaemon) stop() {
	d.t.Helper()
	select {
	case <-d.exited:
		return
	default:
	}
	d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-d.exited:
	case <-time.After(60 * time.Second):
		d.cmd.Process.Kill()
		<-d.exited
		d.t.Fatalf("daemon did not drain:\n%s", d.output())
	}
}

type mrpEnv struct {
	t        *testing.T
	root     string
	dir      string
	bin      string
	venue    *mrpVenue
	socket   *mrpSocket
	venueLog string
	offline  string
	token    string
}

func newMRPEnv(t *testing.T, bars int) *mrpEnv {
	t.Helper()
	root := htfITRoot(t)
	e := &mrpEnv{t: t, root: root, dir: t.TempDir(), bin: pyintegrationBinary(t), venue: newMRPVenue(t, bars), socket: newMRPSocket(t), token: "manual-regime-test-token"}
	e.venueLog = filepath.Join(e.dir, "venue.jsonl")
	e.offline = filepath.Join(e.dir, "offline.json")
	if err := os.WriteFile(e.offline, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *mrpEnv) env(extra ...string) []string {
	env := append(os.Environ(),
		"PYTHONPATH="+filepath.Join(e.root, "shared_scripts", "testdata", "htf_offline"),
		"HTF_OFFLINE_FIXTURE="+e.offline,
		"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=",
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost",
		manualRegimeTestHLInfoEnv+"="+e.venue.server.URL,
		manualRegimeTestHLWSEnv+"="+e.socket.url(),
		"STATUS_AUTH_TOKEN="+e.token,
		manualSchedulerURLEnv+"=",
	)
	return append(env, extra...)
}

func mrpStopBlock() map[string]any {
	return map[string]any{"trend_regime": map[string]any{"trending_up": map[string]any{"atr_multiple": 2}, "trending_down": map[string]any{"atr_multiple": 2}, "ranging": map[string]any{"atr_multiple": 2}}}
}

func mrpManualStrategy(id, window string) map[string]any {
	return map[string]any{
		"id": id, "type": "manual", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
		"symbol": "BTC", "timeframe": "1m", "args": []any{"hold", "BTC", "1m", "--mode=paper"},
		"capital": 1000, "leverage": 1, "max_drawdown_pct": 90, "direction": "both",
		"regime_atr_window": window,
	}
}

func (e *mrpEnv) writeConfig(name string, mutate func(map[string]any)) string {
	e.t.Helper()
	doc := map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(e.dir, "state.db"), "log_dir": filepath.Join(e.dir, "logs"),
		"market_feed": "websocket", "auto_update": "off",
		"regime": map[string]any{"enabled": true, "timeframe": "1m", "windows": map[string]any{
			"short": map[string]any{"classifier": "adx", "period": 14},
			"long":  map[string]any{"classifier": "adx", "period": 50},
		}},
		"strategies": []any{mrpManualStrategy("mr-ready", "short"), mrpManualStrategy("mr-warming", "long")},
	}
	if mutate != nil {
		mutate(doc)
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		e.t.Fatal(err)
	}
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *mrpEnv) startDaemon(cfgPath string, port int, extraEnv ...string) *mrpDaemon {
	e.t.Helper()
	d := &mrpDaemon{t: e.t, port: port, exited: make(chan struct{})}
	args := []string{"--config", cfgPath}
	if port > 0 {
		args = append(args, "--status-port", strconv.Itoa(port))
	}
	d.cmd = exec.Command(e.bin, args...)
	d.cmd.Dir = e.root
	d.cmd.Env = e.env(extraEnv...)
	d.cmd.Stdout = d
	d.cmd.Stderr = d
	if err := d.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go func() {
		d.cmd.Wait()
		close(d.exited)
	}()
	e.t.Cleanup(func() {
		select {
		case <-d.exited:
		default:
			d.cmd.Process.Kill()
			<-d.exited
		}
	})
	return d
}

type mrpRun struct {
	code   int
	stdout string
	stderr string
}

func (r mrpRun) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

func (e *mrpEnv) command(cfgPath string, extraEnv []string, args ...string) mrpRun {
	e.t.Helper()
	full := append([]string{args[0], "--config", cfgPath}, args[1:]...)
	cmd := exec.Command(e.bin, full...)
	cmd.Dir = e.root
	cmd.Env = e.env(append([]string{manualRegimeTestVenueLogEnv + "=" + e.venueLog, manualRegimeTestFillPxEnv + "=" + strconv.FormatFloat(mrpMark(), 'f', 4, 64)}, extraEnv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return mrpRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (e *mrpEnv) venueEvents() []map[string]any {
	e.t.Helper()
	f, err := os.Open(e.venueLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (e *mrpEnv) venueKinds() map[string]int {
	out := map[string]int{}
	for _, ev := range e.venueEvents() {
		out[fmt.Sprint(ev["kind"])]++
	}
	return out
}
