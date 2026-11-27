//go:build pyintegration

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const dcIsoToken = "dashboard-candle-isolation-token"

type dcIsoProc struct {
	*fpProc
	port int
}

func dcIsoStageRoot(t *testing.T, root string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Name() == ".venv" || en.Name() == ".git" {
			continue
		}
		if err := os.Symlink(filepath.Join(root, en.Name()), filepath.Join(dir, en.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "python-argv.log")
	real := filepath.Join(root, ".venv", "bin", "python3")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".venv", "bin", "python3"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, logPath
}

func dcIsoShimLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func dcIsoCandleSpawns(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "fetch_candles.py") && !strings.Contains(l, "--probe-only") {
			n++
		}
	}
	return n
}

func dcIsoArrivalTypes(arr []fpArrival) []string {
	var out []string
	for _, a := range arr {
		out = append(out, a.owner+":"+a.typ)
	}
	return out
}

func dcIsoRequest(t *testing.T, port int, method, path string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+dcIsoToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

type dcIsoFeedView struct {
	Requests  json.RawMessage `json:"requests"`
	RateLimit []struct {
		Host        string `json:"host"`
		State       string `json:"state"`
		PausedUntil string `json:"paused_until"`
		Extensions  int    `json:"extensions"`
		Refused     int    `json:"refused"`
	} `json:"rate_limit"`
	Weight *struct {
		Totals json.RawMessage `json:"totals"`
	} `json:"weight"`
}

type dcIsoStatus struct {
	CycleCount       int            `json:"cycle_count"`
	MarketFeed       *dcIsoFeedView `json:"market_feed"`
	DashboardCandles struct {
		State      string `json:"state"`
		Generation string `json:"generation"`
		Service    struct {
			SourceReads int64 `json:"source_reads"`
			Refused     int64 `json:"refused_busy"`
		} `json:"service"`
	} `json:"dashboard_candles"`
}

func dcIsoReadStatus(t *testing.T, port int) dcIsoStatus {
	t.Helper()
	code, _, raw := dcIsoRequest(t, port, http.MethodGet, "/status", nil)
	if code != 200 {
		t.Fatalf("/status = %d: %s", code, raw)
	}
	var st dcIsoStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	return st
}

func dcIsoFeedFingerprint(st dcIsoStatus) string {
	if st.MarketFeed == nil {
		return "no-feed"
	}
	view := struct {
		Requests  json.RawMessage
		RateLimit any
		Weight    json.RawMessage
	}{Requests: st.MarketFeed.Requests, RateLimit: st.MarketFeed.RateLimit}
	if st.MarketFeed.Weight != nil {
		view.Weight = st.MarketFeed.Weight.Totals
	}
	raw, _ := json.Marshal(view)
	return string(raw)
}

func dcIsoSeedReview(t *testing.T, dbPath string, base time.Time) {
	t.Helper()
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	open, closeAt := base.Add(2*time.Hour+12*time.Minute), base.Add(6*time.Hour+6*time.Minute)
	for _, tr := range []Trade{
		{StrategyID: "hl-btc", Timestamp: open, Symbol: "BTC", PositionID: "pos-iso", Side: "buy", Quantity: 1, Price: 100, Value: 100, TradeType: "perps", ExchangeFee: 0.05},
		{StrategyID: "hl-btc", Timestamp: closeAt, Symbol: "BTC", PositionID: "pos-iso", Side: "sell", Quantity: 1, Price: 110, Value: 110, TradeType: "perps", IsClose: true, RealizedPnL: 10, PnLGross: true, ExchangeFee: 0.05},
	} {
		if err := db.InsertTrade("hl-btc", tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertTradeDiagnostics(&TradeDiagnosticsRow{StrategyID: "hl-btc", PositionID: "pos-iso", Symbol: "BTC", Side: "long", CloseReason: "signal",
		EntryPrice: 100, ExitPrice: 110, Quantity: 1, RealizedPnL: 9.95, EntryATR: 2, OpenedAt: open, ClosedAt: closeAt, MetricsStatus: diagMetricsPending}); err != nil {
		t.Fatal(err)
	}
}

func dcIsoSchedulerConfig(t *testing.T, dir, mode, archive string, port int, sock string) string {
	t.Helper()
	doc := map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 3600, "auto_update": "off", "market_feed": mode,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "status_port": port,
		"dashboard_candles": map[string]any{"archive_dir": archive, "refresh_seconds": 30},
		"strategies": []any{map[string]any{"id": "hl-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []any{"sma_crossover", "BTC", "1h", "--mode=paper"}, "capital": 1000, "max_drawdown_pct": 50}},
	}
	if mode == marketFeedShared {
		doc["shared_market_feed"] = map[string]any{"primary_socket": sock}
	}
	blob, _ := json.Marshal(doc)
	path := filepath.Join(dir, "scheduler-"+mode+".json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dcIsoFeedConfig(t *testing.T, dir, consumer, sock string, port int) string {
	t.Helper()
	blob, _ := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "role": "feed", "status_port": port,
		"feed": map[string]any{"source": "rest", "socket_path": sock, "consumer_configs": []string{consumer},
			"request_budget": map[string]any{"per_minute": 1000, "startup": 1000, "weight_per_minute": 700, "weight_startup": 300, "weight_reserve": 100}}})
	path := filepath.Join(dir, "feed.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dcIsoDrive(t *testing.T, port int, base time.Time, save bool) map[string]int {
	t.Helper()
	codes := map[string]int{}
	var mu sync.Mutex
	record := func(name string, code int) {
		mu.Lock()
		codes[name] = code
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for client := 0; client < 6; client++ {
		wg.Add(1)
		go func(client int) {
			defer wg.Done()
			code, _, raw := dcIsoRequest(t, port, http.MethodGet, "/api/strategies/hl-btc/candles?limit=300", nil)
			record(fmt.Sprintf("latest-%d", client), code)
			var resp candleDisplayResponse
			if code == 200 && json.Unmarshal(raw, &resp) == nil && resp.Page.Older != "" {
				c, _, _ := dcIsoRequest(t, port, http.MethodGet, "/api/strategies/hl-btc/candles?limit=300&before="+url.QueryEscape(resp.Page.Older), nil)
				record(fmt.Sprintf("older-%d", client), c)
			}
			far := base.Add(-20000 * time.Hour).Unix()
			c, _, _ := dcIsoRequest(t, port, http.MethodGet, fmt.Sprintf("/api/strategies/hl-btc/candles?from=%d&to=%d&limit=50", far+int64(client)*3600, far+int64(client)*3600+7200), nil)
			record(fmt.Sprintf("miss-%d", client), c)
			c, _, _ = dcIsoRequest(t, port, http.MethodGet, "/api/strategies/hl-btc/candles?timeframe=4h&limit=100", nil)
			record(fmt.Sprintf("4h-%d", client), c)
		}(client)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, _, _ := dcIsoRequest(t, port, http.MethodPost, "/api/strategies/hl-btc/simulate", []byte(`{"limit":200}`))
		record("preview", c)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, _, _ := dcIsoRequest(t, port, http.MethodGet, "/api/strategies/hl-btc/trade-review?position_id=pos-iso", nil)
		record("review", c)
		if save {
			body, _ := json.Marshal(tradeFeedbackRequest{PositionID: "pos-iso", EntryTime: base.Add(2 * time.Hour).Unix(), ExitTime: base.Add(6 * time.Hour).Unix(), EntryPrice: 95, ExitPrice: 115, Note: "isolation"})
			c, _, raw := dcIsoRequest(t, port, http.MethodPost, "/api/strategies/hl-btc/trade-feedback", body)
			if c != 200 {
				t.Logf("review save = %d: %s", c, raw)
			}
			record("save", c)
		}
	}()
	wg.Wait()
	return codes
}

func TestDashboardCandleIsolationProcesses(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{marketFeedREST, marketFeedWebsocket, marketFeedShared} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			e := newFPEnv(t)
			if _, err := os.Stat(filepath.Join(e.root, ".venv", "bin", "python3")); err != nil {
				t.Skipf("repository virtualenv missing (%v); run uv sync", err)
			}
			stage, shimLog := dcIsoStageRoot(t, e.root)
			archive := t.TempDir()
			now := time.Now().UTC()
			latest := now.Truncate(time.Hour).Add(-time.Hour)
			base := latest.Add(-200 * time.Hour)
			writeTestCandleArchive(t, archive, testArchiveSpec{Series: []testArchiveSeries{
				{ID: hlTestIdentity("BTC", "1h"), Bars: testArchiveBars(latest.Add(-3000*time.Hour), latest, time.Hour), ChunkBars: 512},
				{ID: hlTestIdentity("BTC", "4h"), Bars: testArchiveBars(latest.Add(-3000*time.Hour).Truncate(4*time.Hour), latest.Truncate(4*time.Hour).Add(-4*time.Hour), 4*time.Hour)},
			}})
			port := htfITFreePort(t)
			feedPort := 0
			if mode == marketFeedShared {
				feedPort = htfITFreePort(t)
			}
			feedPrint := func() string {
				if feedPort == 0 {
					return ""
				}
				return dcIsoFeedFingerprint(dcIsoReadStatus(t, feedPort))
			}
			sock := filepath.Join(e.dir, "feed-"+mode+".sock")
			cfg := dcIsoSchedulerConfig(t, e.dir, mode, archive, port, sock)
			dcIsoSeedReview(t, filepath.Join(e.dir, "state.db"), base)
			extra := []string{"STATUS_AUTH_TOKEN=" + dcIsoToken, "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=", "HYPERLIQUID_TESTNET="}
			if mode == marketFeedWebsocket {
				socket := newMRPSocket(t)
				extra = append(extra, manualRegimeTestHLWSEnv+"="+socket.url())
				e.venue.mu.Lock()
				e.venue.status["candleSnapshot"] = http.StatusTooManyRequests
				e.venue.mu.Unlock()
			}
			if mode == marketFeedShared {
				feedCfg := dcIsoFeedConfig(t, e.dir, cfg, sock, feedPort)
				e.install(t, feedCfg)
				feed := e.start("feed", feedCfg)
				feed.waitFor("[feed] serving:", 180*time.Second)
			}
			p := &fpProc{t: t, name: "scheduler-" + mode, exited: make(chan struct{})}
			p.cmd = newDCIsoCommand(e, stage, cfg, extra)
			p.cmd.Stdout, p.cmd.Stderr = p, p
			if err := p.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() {
				p.cmd.Wait()
				close(p.exited)
			}()
			t.Cleanup(p.stop)
			p.waitFor("[server] Status endpoint at", 120*time.Second)
			st0 := dcIsoReadStatus(t, port)
			if st0.DashboardCandles.State == "" {
				t.Fatalf("/status has no dashboard_candles block")
			}
			p.waitFor("[candle-archive] admitted generation", 120*time.Second)
			deadline := time.Now().Add(240 * time.Second)
			for dcIsoReadStatus(t, port).CycleCount < 1 {
				if time.Now().After(deadline) {
					t.Fatalf("no cycle finished:\n%s", p.output())
				}
				time.Sleep(250 * time.Millisecond)
			}
			time.Sleep(2 * time.Second)

			shimA := dcIsoShimLines(t, shimLog)
			var venueB, quietVenue, quietSpawns int
			var shimB []string
			var stB dcIsoStatus
			var feedB string
			for attempt := 1; ; attempt++ {
				stA, feedA := dcIsoReadStatus(t, port), feedPrint()
				venueA := len(e.venue.snapshot())
				time.Sleep(2 * time.Second)
				venueB, shimB = len(e.venue.snapshot()), dcIsoShimLines(t, shimLog)
				stB, feedB = dcIsoReadStatus(t, port), feedPrint()
				quietVenue, quietSpawns = venueB-venueA, dcIsoCandleSpawns(shimB)-dcIsoCandleSpawns(shimA)
				if quietVenue == 0 && dcIsoFeedFingerprint(stA) == dcIsoFeedFingerprint(stB) && feedA == feedB {
					break
				}
				moved := fmt.Sprintf("the venue or the feed moved %d requests (%v) in a quiet interval; the window cannot attribute traffic\n%s\n%s\nfeed %s\nfeed %s",
					quietVenue, dcIsoArrivalTypes(e.venue.snapshot()[venueA:venueB]), dcIsoFeedFingerprint(stA), dcIsoFeedFingerprint(stB), feedA, feedB)
				if attempt == 5 {
					t.Fatalf("fixture: %s", moved)
				}
				t.Logf("quiet window %d: %s", attempt, moved)
			}
			venueB = len(e.venue.snapshot())
			shimB = dcIsoShimLines(t, shimLog)

			codes := dcIsoDrive(t, port, base, true)
			venueC, shimC := len(e.venue.snapshot()), dcIsoShimLines(t, shimLog)
			stC, feedC := dcIsoReadStatus(t, port), feedPrint()
			for name, code := range codes {
				if code != 200 {
					t.Errorf("%s = %d", name, code)
				}
			}
			dashVenue := venueC - venueB
			dashSpawns := dcIsoCandleSpawns(shimC) - dcIsoCandleSpawns(shimB)
			if dashVenue != 0 || dashSpawns != 0 || quietSpawns != 0 {
				t.Fatalf("dashboard candle traffic moved the venue by %d requests (%v) and spawned fetch_candles.py %d times", dashVenue,
					dcIsoArrivalTypes(e.venue.snapshot()[venueB:venueC]), dashSpawns)
			}
			if a, b := dcIsoFeedFingerprint(stB), dcIsoFeedFingerprint(stC); a != b || feedB != feedC {
				t.Fatalf("dashboard traffic changed the feed request, weight or pause state:\nbefore %s\nafter  %s\nfeed before %s\nfeed after  %s", a, b, feedB, feedC)
			}
			paused := false
			if stC.MarketFeed != nil {
				for _, h := range stC.MarketFeed.RateLimit {
					if h.State == "paused" && h.PausedUntil != "" {
						paused = true
					}
				}
			}
			if mode == marketFeedWebsocket && !paused {
				t.Fatalf("fixture: the websocket pass needs an active host pause during the dashboard window: %s", dcIsoFeedFingerprint(stC))
			}
			if stC.DashboardCandles.Service.SourceReads == 0 {
				t.Fatalf("the dashboard served no source reads; the drive did not exercise the service")
			}
			t.Logf("mode=%s venue_delta=%d fetch_candles_spawns=%d quiet_venue_delta=%d host_pause_active=%v source_reads=%d codes=%v", mode, dashVenue, dashSpawns, quietVenue, paused, stC.DashboardCandles.Service.SourceReads, codes)
		})
	}
}

func newDCIsoCommand(e *fpEnv, stage, cfg string, extra []string) *exec.Cmd {
	cmd := exec.Command(e.bin, "--config", cfg)
	cmd.Dir = stage
	cmd.Env = e.processEnv("scheduler", extra...)
	return cmd
}

func TestDashboardCandleIsolationMissingArchiveKeepsStartup(t *testing.T) {
	t.Parallel()
	e := newFPEnv(t)
	if _, err := os.Stat(filepath.Join(e.root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	port := htfITFreePort(t)
	missing := filepath.Join(e.dir, "no-archive-here")
	cfg := dcIsoSchedulerConfig(t, e.dir, marketFeedREST, missing, port, "")
	p := e.start("scheduler-missing", cfg, "STATUS_AUTH_TOKEN="+dcIsoToken, "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
	p.waitFor("[server] Status endpoint at", 120*time.Second)
	deadline := time.Now().Add(240 * time.Second)
	for {
		st := dcIsoReadStatus(t, port)
		if st.CycleCount >= 1 {
			if st.DashboardCandles.State != candleArchiveStateRefused {
				t.Fatalf("a missing archive reports state %q, want %q", st.DashboardCandles.State, candleArchiveStateRefused)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no cycle finished with a missing archive:\n%s", p.output())
		}
		time.Sleep(250 * time.Millisecond)
	}
	code, _, raw := dcIsoRequest(t, port, http.MethodGet, "/api/strategies/hl-btc/candles", nil)
	var resp candleDisplayResponse
	if code != 200 || json.Unmarshal(raw, &resp) != nil || resp.MarketAvailability == nil || resp.MarketAvailability.Code != candleAvailSourceMissing {
		t.Fatalf("display with a missing archive = %d: %s", code, raw)
	}
	if code, _, raw := dcIsoRequest(t, port, http.MethodPost, "/api/strategies/hl-btc/simulate", []byte(`{"limit":100}`)); code != http.StatusServiceUnavailable {
		t.Fatalf("preview with a missing archive = %d: %s", code, raw)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["dashboard_candles"] = map[string]any{"archive_dir": filepath.Join(e.dir, "another-archive")}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	p.waitFor("dashboard_candles changed", 60*time.Second)
	if st := dcIsoReadStatus(t, port); st.DashboardCandles.State != candleArchiveStateRefused {
		t.Fatalf("a refused reload changed the archive state to %q", st.DashboardCandles.State)
	}
}
