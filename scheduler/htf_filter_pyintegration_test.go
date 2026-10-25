//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const htfITHour = int64(3_600_000)

func htfITRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	return root
}

func htfITNow() int64 {
	n := time.Now().UTC().UnixMilli()
	return n - n%htfITHour
}

func htfITBars(n int, step, end int64, last string) [][]any {
	start := end - int64(n-1)*step
	out := make([][]any, 0, n)
	p := 100.0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			p += 0.4
		} else {
			p -= 0.35
		}
		o, h, l, c := p-0.1, p+0.5, p-0.5, p
		if i == n-1 {
			switch last {
			case "up":
				h, c = p+12.5, p+12
			case "down":
				l, c = p-12.5, p-12
			case "tp":
				h, c = p+30.5, p+30
			}
		}
		out = append(out, []any{start + int64(i)*step, o, h, l, c, 1000 + float64(i)})
	}
	return out
}

func htfITHTF(mode string, now int64) any {
	step := 4 * htfITHour
	switch mode {
	case "short":
		return htfITBars(49, step, now, "")
	case "error":
		return "network_error"
	case "transient":
		return map[string]any{"error": "binanceus GET https://api.binance.us/api/v3/klines 503 Service Unavailable: bad gateway"}
	case "nan":
		rows := htfITBars(60, step, now, "")
		for _, r := range rows {
			r[4] = "nan"
		}
		return rows
	case "neutral":
		out := make([][]any, 0, 60)
		for i := 0; i < 60; i++ {
			out = append(out, []any{now - int64(59-i)*step, 100.0, 100.0, 100.0, 100.0, 1.0})
		}
		return out
	}
	panic("unknown htf fixture mode " + mode)
}

func htfITHourlyHTF(mode string, now int64) any {
	step := 4 * htfITHour
	switch mode {
	case "error":
		return "network_error"
	case "transient":
		return map[string]any{"error": "yahoo 1h 503 Service Unavailable: bad gateway"}
	}
	forming := now - now%step
	buckets := int64(60)
	if mode == "short" {
		buckets = 49
	}
	hours := int((now-(forming-(buckets-1)*step))/htfITHour) + 1
	rows := htfITBars(hours, htfITHour, now, "")
	switch mode {
	case "short":
		return rows
	case "nan":
		for _, r := range rows {
			r[4] = "nan"
		}
		return rows
	case "neutral":
		for _, r := range rows {
			r[1], r[2], r[3], r[4], r[5] = 100.0, 100.0, 100.0, 100.0, 1.0
		}
		return rows
	}
	panic("unknown htf fixture mode " + mode)
}

type htfITFixture struct {
	HTF        string
	Spot       string
	Fut        string
	NoPrimary  bool
	MarketOpen *bool
}

func htfITLastClose(rows [][]any) float64 {
	return rows[len(rows)-1][4].(float64)
}

func htfITWriteFixture(t *testing.T, path string, f htfITFixture) string {
	t.Helper()
	now := htfITNow()
	spot := htfITBars(200, htfITHour, now, f.Spot)
	fut := htfITBars(200, htfITHour, now, f.Fut)
	htf := htfITHTF(f.HTF, now)
	var spotRows, futRows any = spot, fut
	if f.NoPrimary {
		spotRows, futRows = [][]any{}, [][]any{}
	}
	open := true
	if f.MarketOpen != nil {
		open = *f.MarketOpen
	}
	doc := map[string]any{
		"ccxt": map[string]any{
			"BTC/USDT|1h": spotRows, "BTC/USDT|4h": htf,
			"BTC/USDT:USDT|1h": futRows, "BTC/USDT:USDT|4h": htf,
		},
		"tickers": map[string]any{"BTC/USDT": htfITLastClose(spot), "BTC/USDT:USDT": htfITLastClose(fut)},
		"yahoo": map[string]any{
			"BTC-USD|30m": spotRows, "BTC-USD|1h": htfITHourlyHTF(f.HTF, now),
			"ES=F|1h": futRows, "ES=F|4h": htf,
		},
		"yahoo_price":         map[string]any{"BTC-USD": htfITLastClose(spot), "ES=F": htfITLastClose(fut)},
		"topstep_market_open": open,
	}
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func htfITUseFixture(t *testing.T, root, path string) {
	t.Helper()
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "htf_offline"))
	t.Setenv("HTF_OFFLINE_FIXTURE", path)
}

func htfITFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func htfITPositions(t *testing.T, log, id string) int {
	t.Helper()
	re := regexp.MustCompile(`\[` + regexp.QuoteMeta(id) + `\] \[INFO\] Status: [^\n]*\| positions=(\d+) \|`)
	m := re.FindAllStringSubmatch(log, -1)
	if len(m) != 1 {
		t.Fatalf("want one status line for %s, got %d\n%s", id, len(m), log)
	}
	n, _ := strconv.Atoi(m[0][1])
	return n
}

func htfITHeld(log, id string) bool {
	return strings.Contains(log, "["+id+"] [WARN] HTF filter: BUY signal suppressed — higher-timeframe 4h input unavailable")
}

func TestHTFFilterIntegrationSchedulerOnceHoldsEntriesAndKeepsCloses(t *testing.T) {
	t.Parallel()
	root := htfITRoot(t)
	dir := t.TempDir()
	bin := pyintegrationBinary(t)
	type strat struct {
		id, symbol string
		spot       bool
		entry      map[string]any
	}
	strats := []strat{
		{"bus-atrb-btc", "BTC/USDT", true, map[string]any{"type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py", "args": []string{"atr_breakout", "BTC/USDT", "1h"}, "capital": 1000}},
		{"okx-atrb-btc", "BTC", true, map[string]any{"type": "spot", "platform": "okx", "script": "shared_scripts/check_okx.py", "args": []string{"atr_breakout", "BTC", "1h", "--mode=paper", "--inst-type=spot"}, "capital": 1000}},
		{"okx-brk-btc-perp", "BTC", false, map[string]any{"type": "perps", "platform": "okx", "script": "shared_scripts/check_okx.py", "args": []string{"breakout", "BTC", "1h", "--mode=paper", "--inst-type=swap"}, "capital": 1000, "leverage": 1, "sizing_leverage": 1, "close_strategy": map[string]any{"name": "tiered_tp_pct"}}},
		{"rh-atrb-btc", "BTC", true, map[string]any{"type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py", "args": []string{"atr_breakout", "BTC", "30m", "--mode=paper"}, "capital": 1000}},
		{"ts-brk-es", "ES", false, map[string]any{"type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py", "args": []string{"breakout", "ES", "1h", "--mode=paper"}, "capital": 50000, "close_strategy": map[string]any{"name": "tiered_tp_pct"}}},
	}
	var entries []any
	for _, s := range strats {
		e := map[string]any{"id": s.id, "max_drawdown_pct": 90, "interval_seconds": 60, "htf_filter": true}
		for k, v := range s.entry {
			e[k] = v
		}
		entries = append(entries, e)
	}
	cfgPath := filepath.Join(dir, "config.json")
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "strategies": entries,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(htfITFreePort(t))
	run := func(name string, f htfITFixture) string {
		t.Helper()
		fixture := htfITWriteFixture(t, filepath.Join(dir, name+".json"), f)
		cmd := exec.Command(bin, "--config", cfgPath, "--once", "--status-port", port)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"),
			"HTF_OFFLINE_FIXTURE="+fixture,
			"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
		out, err := cmd.CombinedOutput()
		log := string(out)
		if err != nil {
			t.Fatalf("%s: scheduler --once failed: %v\n%s", name, err, log)
		}
		if strings.Contains(log, "network blocked by the htf offline fixture") {
			t.Fatalf("%s: a check tried an uncontrolled network call\n%s", name, log)
		}
		if strings.Contains(log, "HTF FILTER UNAVAILABLE") {
			t.Fatalf("%s: a fresh process has no unavailable history, so no alert may fire\n%s", name, log)
		}
		t.Logf("%s log:\n%s", name, log)
		return log
	}
	pythonLine := func(log string, s strat) bool {
		return strings.Contains(log, "HTF filter unavailable for "+s.symbol+" 4h: ")
	}

	flatHeld := run("flat-short-htf", htfITFixture{HTF: "short", Spot: "up", Fut: "up"})
	for _, s := range strats {
		if !htfITHeld(flatHeld, s.id) || !pythonLine(flatHeld, s) || !strings.Contains(flatHeld, "49 4h candles available for "+s.symbol+" (need 50)") {
			t.Errorf("%s: a flat entry with a 49-row HTF frame must log the Python unavailable line and the Go hold", s.id)
		}
		if n := htfITPositions(t, flatHeld, s.id); n != 0 {
			t.Errorf("%s: the held entry opened %d position(s)", s.id, n)
		}
	}

	neutral := run("flat-neutral-htf", htfITFixture{HTF: "neutral", Spot: "up", Fut: "up"})
	for _, s := range strats {
		if strings.Contains(neutral, "["+s.id+"] [WARN] HTF filter:") {
			t.Errorf("%s: a successful neutral HTF trend must pass the entry", s.id)
		}
		if n := htfITPositions(t, neutral, s.id); n != 1 {
			t.Errorf("%s: a neutral HTF trend must keep the entry signal, got %d position(s)", s.id, n)
		}
	}

	addition := run("long-error-htf", htfITFixture{HTF: "error", Spot: "up", Fut: "up"})
	for _, s := range strats {
		if !pythonLine(addition, s) {
			t.Errorf("%s: a failed HTF fetch must report unavailable", s.id)
		}
		if s.spot && !htfITHeld(addition, s.id) {
			t.Errorf("%s: a BUY addition to a long must be held while HTF input is unavailable", s.id)
		}
		if n := htfITPositions(t, addition, s.id); n != 1 {
			t.Errorf("%s: the open position must stay, got %d", s.id, n)
		}
	}

	closes := run("long-nan-htf-closes", htfITFixture{HTF: "nan", Spot: "down", Fut: "tp"})
	for _, s := range strats {
		nonFinite := "4h close values for " + s.symbol + " are not finite"
		if s.id == "rh-atrb-btc" {
			nonFinite = "no 4h candle frame was returned for " + s.symbol
		}
		if !pythonLine(closes, s) || !strings.Contains(closes, nonFinite) {
			t.Errorf("%s: non-finite HTF closes must report unavailable", s.id)
		}
		if strings.Contains(closes, "["+s.id+"] [WARN] HTF filter:") {
			t.Errorf("%s: a reducing signal must pass the HTF hold", s.id)
		}
		if !strings.Contains(closes, "["+s.id+"] [INFO] SELL ") {
			t.Errorf("%s: the close must execute while HTF input is unavailable", s.id)
		}
		if n := htfITPositions(t, closes, s.id); n != 0 {
			t.Errorf("%s: the close must flatten the position, got %d", s.id, n)
		}
	}
}

const htfITStubScript = `import os, sys
sys.stdout.write(os.environ.get("HTF_IT_STUB_JSON", ""))
sys.stdout.flush()
sys.exit(int(os.environ.get("HTF_IT_STUB_EXIT", "0")))
`

type htfITConsumer struct {
	name string
	sc   StrategyConfig
	base map[string]any
	run  func(sc StrategyConfig, logger *StrategyLogger) bool
}

func htfITConsumers(stub string) []htfITConsumer {
	prices := map[string]float64{"BTC/USDT": 100, "BTC": 100, "ES": 100}
	notifier := NewMultiNotifier()
	base := func(platform string, extra map[string]any) map[string]any {
		out := map[string]any{"strategy": "breakout", "symbol": "BTC", "timeframe": "1h", "signal": 1, "price": 100,
			"indicators": map[string]any{}, "mode": "paper", "platform": platform, "close_fraction": 0}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []htfITConsumer{
		{"binanceus spot", StrategyConfig{ID: "htf-c-spot", Type: "spot", Platform: "binanceus", Script: stub, Args: []string{"breakout", "BTC/USDT", "1h"}, HTFFilter: true},
			base("binanceus", map[string]any{"symbol": "BTC/USDT"}),
			func(sc StrategyConfig, l *StrategyLogger) bool {
				_, _, _, ok := runSpotCheck(sc, prices, PositionCtx{}, nil, "simple", notifier, l)
				return ok
			}},
		{"okx spot", StrategyConfig{ID: "htf-c-okx", Type: "spot", Platform: "okx", Script: stub, Args: []string{"breakout", "BTC", "1h", "--mode=paper", "--inst-type=spot"}, HTFFilter: true},
			base("okx", nil),
			func(sc StrategyConfig, l *StrategyLogger) bool {
				_, _, _, ok := runOKXCheck(sc, prices, PositionCtx{}, nil, "simple", notifier, l)
				return ok
			}},
		{"robinhood spot", StrategyConfig{ID: "htf-c-rh", Type: "spot", Platform: "robinhood", Script: stub, Args: []string{"breakout", "BTC", "1h", "--mode=paper"}, HTFFilter: true},
			base("robinhood", nil),
			func(sc StrategyConfig, l *StrategyLogger) bool {
				_, _, _, ok := runRobinhoodCheck(sc, prices, PositionCtx{}, nil, "simple", notifier, l)
				return ok
			}},
		{"topstep futures", StrategyConfig{ID: "htf-c-ts", Type: "futures", Platform: "topstep", Script: stub, Args: []string{"breakout", "ES", "1h", "--mode=paper"}, HTFFilter: true},
			base("topstep", map[string]any{"symbol": "ES", "market_open": true, "contract_spec": map[string]any{"tick_size": 0.25, "tick_value": 12.5, "multiplier": 50, "margin": 1000}}),
			func(sc StrategyConfig, l *StrategyLogger) bool {
				_, _, _, ok := runTopStepCheck(sc, prices, PositionCtx{}, nil, "simple", notifier, l)
				return ok
			}},
		{"hyperliquid perps", StrategyConfig{ID: "htf-c-hl", Type: "perps", Platform: "hyperliquid", Script: stub, Args: []string{"breakout", "BTC", "1h", "--mode=paper"}, HTFFilter: true},
			base("hyperliquid", nil),
			func(sc StrategyConfig, l *StrategyLogger) bool {
				_, _, _, ok := runHyperliquidCheck(&sc, prices, PositionCtx{}, nil, "simple", notifier, l, nil, nil)
				return ok
			}},
	}
}

func htfITStubRun(t *testing.T, c htfITConsumer, sc StrategyConfig, payload map[string]any, exit int) (bool, string) {
	t.Helper()
	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTF_IT_STUB_JSON", string(blob))
	t.Setenv("HTF_IT_STUB_EXIT", strconv.Itoa(exit))
	var buf bytes.Buffer
	ok := c.run(sc, &StrategyLogger{stratID: sc.ID, writer: &buf})
	return ok, buf.String()
}

func htfITWith(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func htfITBlock(status string, trend int, reason string) map[string]any {
	return map[string]any{"status": status, "timeframe": "4h", "trend": trend, "reason": reason}
}

func TestHTFFilterIntegrationConsumersValidateTheContract(t *testing.T) {
	root := htfITRoot(t)
	t.Chdir(root)
	stub := filepath.Join(t.TempDir(), "htf_stub.py")
	if err := os.WriteFile(stub, []byte(htfITStubScript), 0o600); err != nil {
		t.Fatal(err)
	}
	resetHTFFilterTrackersForTest(t)
	for _, c := range htfITConsumers(stub) {
		t.Run(c.name, func(t *testing.T) {
			if ok, log := htfITStubRun(t, c, c.sc, c.base, 0); ok || !strings.Contains(log, "returned no htf_filter block") {
				t.Fatalf("a missing block must be a script error, ok=%t\n%s", ok, log)
			}
			if ok, log := htfITStubRun(t, c, c.sc, htfITWith(c.base, map[string]any{"htf_filter": htfITBlock("maybe", 0, "x")}), 0); ok || !strings.Contains(log, `unknown htf_filter status "maybe"`) {
				t.Fatalf("an unknown status must be a script error, ok=%t\n%s", ok, log)
			}
			if ok, log := htfITStubRun(t, c, c.sc, htfITWith(c.base, map[string]any{"htf_filter": htfITBlock("ok", 2, "")}), 0); ok || !strings.Contains(log, "reports trend 2") {
				t.Fatalf("an out-of-range trend must be a script error, ok=%t\n%s", ok, log)
			}
			if ok, log := htfITStubRun(t, c, c.sc, htfITWith(c.base, map[string]any{"htf_filter": htfITBlock("not_evaluated", 0, "earlier hold")}), 0); ok || !strings.Contains(log, "cannot authorize this signal") {
				t.Fatalf("an unsupported not_evaluated context must be a script error, ok=%t\n%s", ok, log)
			}
			if ok, log := htfITStubRun(t, c, c.sc, htfITWith(c.base, map[string]any{"htf_filter": htfITBlock("unavailable", 0, "49 4h candles available for BTC (need 50)")}), 0); !ok || strings.Contains(log, "mismatch") {
				t.Fatalf("a valid unavailable result must reach the entry gate, ok=%t\n%s", ok, log)
			}
			if htfITTrackerCount(c.sc.ID) != 1 {
				t.Fatalf("a consumed unavailable result must advance the HTF tracker once, got %d", htfITTrackerCount(c.sc.ID))
			}
			errPayload := htfITWith(c.base, map[string]any{"error": "venue said no", "signal": 0, "price": 0})
			if ok, log := htfITStubRun(t, c, c.sc, errPayload, 1); ok || !strings.Contains(log, "venue said no") || strings.Contains(log, "htf_filter") {
				t.Fatalf("an error payload must keep its own failure handling, ok=%t\n%s", ok, log)
			}
			if htfITTrackerCount(c.sc.ID) != 1 {
				t.Fatalf("a script error must leave the HTF tracker unchanged, got %d", htfITTrackerCount(c.sc.ID))
			}
			off := c.sc
			off.HTFFilter = false
			if ok, log := htfITStubRun(t, c, off, htfITWith(c.base, map[string]any{"htf_filter": htfITBlock("ok", 1, "")}), 0); ok || !strings.Contains(log, "htf_filter is off") {
				t.Fatalf("a block for a disabled filter must be a script error, ok=%t\n%s", ok, log)
			}
			if ok, log := htfITStubRun(t, c, off, c.base, 0); !ok {
				t.Fatalf("a disabled filter keeps the current contract\n%s", log)
			}
		})
	}

	consumers := htfITConsumers(stub)
	ts := consumers[3]
	closed := htfITWith(ts.base, map[string]any{"market_open": false, "signal": 0, "price": 0,
		"htf_filter": htfITBlock("not_evaluated", 0, "the market is closed, so the check returned before fetching candles")})
	if ok, log := htfITStubRun(t, ts, ts.sc, closed, 0); ok || strings.Contains(log, "mismatch") || !strings.Contains(log, "Market closed") {
		t.Fatalf("TopStep's closed-market return is a valid not_evaluated context, ok=%t\n%s", ok, log)
	}
	omitted := htfITWith(closed, map[string]any{"market_open": nil})
	if ok, log := htfITStubRun(t, ts, ts.sc, omitted, 0); ok || !strings.Contains(log, "cannot authorize this signal") {
		t.Fatalf("an omitted market_open cannot prove a closed market, ok=%t\n%s", ok, log)
	}
	closedSignal := htfITWith(closed, map[string]any{"signal": 1})
	if ok, log := htfITStubRun(t, ts, ts.sc, closedSignal, 0); ok || !strings.Contains(log, "cannot authorize this signal") {
		t.Fatalf("a closed-market context with a signal cannot authorize it, ok=%t\n%s", ok, log)
	}

	spot := consumers[0]
	cb := spot.sc
	cb.ClosedBarDecisions = true
	heldPayload := htfITWith(spot.base, map[string]any{"signal": 0,
		"closed_bar_decision": map[string]any{"enabled": true, "held": true, "hold_reason": "no bar in the frame closed at or before the cutoff", "cutoff_ms": 1},
		"htf_filter":          htfITBlock("not_evaluated", 0, "the closed-bar decision held before the higher-timeframe filter ran")})
	if ok, log := htfITStubRun(t, spot, cb, heldPayload, 0); !ok || strings.Contains(log, "mismatch") {
		t.Fatalf("a held closed-bar decision is a valid not_evaluated context, ok=%t\n%s", ok, log)
	}
	if ok, log := htfITStubRun(t, spot, cb, htfITWith(heldPayload, map[string]any{"closed_bar_decision": map[string]any{"enabled": true, "held": false, "cutoff_ms": 1}}), 0); ok || !strings.Contains(log, "cannot authorize this signal") {
		t.Fatalf("an unheld closed-bar decision cannot support not_evaluated, ok=%t\n%s", ok, log)
	}

	hl := consumers[4]
	sc := hl.sc
	var buf bytes.Buffer
	degraded := degradedHyperliquidResult(sc, "BTC", "paper", "no ready higher-timeframe frame for BTC|4h", 100)
	res, _, _, ok := finishHyperliquidCheck(&sc, map[string]float64{"BTC": 100}, PositionCtx{}, nil, NewMultiNotifier(), &StrategyLogger{stratID: sc.ID, writer: &buf}, degraded, "", "", scriptFailureError, false)
	if !ok || res == nil || res.Degraded == "" || strings.Contains(buf.String(), "htf_filter") {
		t.Fatalf("a Go-created degraded result must keep its protection path, ok=%t\n%s", ok, buf.String())
	}
	if held, _ := htfFilterHoldsSignal(sc, res.StrategyDecisionFields, 1, 0, 0, "", true, true); !held {
		t.Fatal("a degraded result carries no filter result, so a position increase must stay held")
	}
}

func resetHTFFilterTrackersForTest(t *testing.T) {
	t.Helper()
	prev, prevTransient := htfFilterTracker, htfFilterTransientTracker
	htfFilterTracker, htfFilterTransientTracker = &ScriptFailureTracker{}, &ScriptFailureTracker{}
	t.Cleanup(func() { htfFilterTracker, htfFilterTransientTracker = prev, prevTransient })
}

func htfITTrackerCount(id string) int {
	total := 0
	for _, tr := range []*ScriptFailureTracker{htfFilterTracker, htfFilterTransientTracker} {
		tr.mu.Lock()
		if e := tr.entries[id]; e != nil {
			total += e.count
		}
		tr.mu.Unlock()
	}
	return total
}

type htfITSink struct {
	mu       sync.Mutex
	messages []mockMessage
	dms      []mockDM
}

func (s *htfITSink) SendMessage(channelID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, mockMessage{channelID, content})
	return nil
}

func (s *htfITSink) SendDM(userID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dms = append(s.dms, mockDM{userID, content})
	return nil
}

func (s *htfITSink) AskDM(string, string, time.Duration) (string, error) { return "", nil }

func (s *htfITSink) Close() {}

func (s *htfITSink) drain() ([]mockMessage, []mockDM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, d := s.messages, s.dms
	s.messages, s.dms = nil, nil
	return m, d
}

func TestHTFFilterIntegrationAlertsRouteThrottleAndRecover(t *testing.T) {
	root := htfITRoot(t)
	t.Chdir(root)
	resetHTFFilterTrackersForTest(t)
	clock := &simulatedClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	if err := bindTradingClock("htf-alert-test", clock.Now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock("htf-alert-test") })
	advance := func(d time.Duration) {
		if err := clock.Set(clock.Now().Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	allowNoEdge := true
	src := StrategyConfig{ID: "htf-src", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"atr_breakout", "BTC/USDT", "1h"}, HTFFilter: true, PaperSource: "src-a"}
	live := StrategyConfig{ID: "htf-live", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"atr_breakout", "BTC/USDT", "1h", "--mode=live"}, HTFFilter: true, AllowNoEdge: &allowNoEdge}
	paperPeer := StrategyConfig{ID: "htf-paper", Type: "spot", Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"atr_breakout", "BTC", "1h", "--mode=paper", "--inst-type=spot"}}
	cfg := &Config{Strategies: []StrategyConfig{src, live, paperPeer}}
	cfg.Discord.Channels = map[string]string{"binanceus": "ch-live", "okx-paper": "ch-okx-paper", "binanceus-paper:src-a": "ch-src-a"}
	sink := &htfITSink{}
	notifier := NewMultiNotifier(notifierBackend{notifier: sink, channels: cfg.Discord.Channels, ownerID: "owner"})
	notifier.ReloadConfig(cfg)
	prices := map[string]float64{"BTC/USDT": 100}
	var logBuf bytes.Buffer
	fixtures := map[string]string{}
	fixture := func(f htfITFixture) string {
		key := fmt.Sprintf("%+v", f)
		if p, ok := fixtures[key]; ok {
			return p
		}
		p := htfITWriteFixture(t, filepath.Join(dir, fmt.Sprintf("f%d.json", len(fixtures))), f)
		fixtures[key] = p
		return p
	}
	spot := func(sc StrategyConfig, f htfITFixture) bool {
		t.Helper()
		htfITUseFixture(t, root, fixture(f))
		advance(5 * time.Minute)
		_, _, _, ok := runSpotCheck(sc, prices, PositionCtx{}, nil, "simple", notifier, &StrategyLogger{stratID: sc.ID, writer: &logBuf})
		return ok
	}
	short := htfITFixture{HTF: "short", Spot: "up", Fut: "up"}
	errFix := htfITFixture{HTF: "error", Spot: "up", Fut: "up"}
	neutral := htfITFixture{HTF: "neutral", Spot: "up", Fut: "up"}
	transient := htfITFixture{HTF: "transient", Spot: "up", Fut: "up"}
	expectNone := func(step string) {
		t.Helper()
		if m, d := sink.drain(); len(m)+len(d) != 0 {
			t.Fatalf("%s: want no alert, got messages=%v dms=%v", step, m, d)
		}
	}
	expectOne := func(step, channel, want string) string {
		t.Helper()
		m, d := sink.drain()
		if len(m) != 1 || m[0].channelID != channel || !strings.Contains(m[0].content, want) {
			t.Fatalf("%s: want one %q message on %s, got %v", step, want, channel, m)
		}
		if len(d) != 1 || d[0].userID != "owner" || !strings.Contains(d[0].content, want) {
			t.Fatalf("%s: want one owner DM, got %v", step, d)
		}
		return m[0].content + "\n" + d[0].content
	}

	for i := 1; i <= 2; i++ {
		if !spot(src, short) {
			t.Fatalf("unavailable evaluation %d must be a usable result\n%s", i, logBuf.String())
		}
		expectNone(fmt.Sprintf("unavailable evaluation %d", i))
	}
	htfITUseFixture(t, root, fixture(htfITFixture{HTF: "short", Spot: "up", Fut: "up", NoPrimary: true}))
	if _, _, _, ok := runSpotCheck(src, prices, PositionCtx{}, nil, "simple", notifier, &StrategyLogger{stratID: src.ID, writer: &logBuf}); ok {
		t.Fatal("a missing primary frame must stay a script error")
	}
	expectNone("script error between unavailable evaluations")
	if htfITTrackerCount(src.ID) != 2 {
		t.Fatalf("a script error must not advance or clear the HTF tracker, got %d", htfITTrackerCount(src.ID))
	}
	spot(src, short)
	text := expectOne("third unavailable evaluation", "ch-src-a", "**HTF FILTER UNAVAILABLE** [htf-src] binanceus")
	for _, want := range []string{"partition=paper:src-a", "HTF=4h", "3 unavailable evaluations", "49 4h candles available for BTC/USDT (need 50)", "new entries and additions are held while filter input is unavailable", "[paper:src-a scope]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the alert must carry %q:\n%s", want, text)
		}
	}
	for i := 4; i <= 9; i++ {
		spot(src, short)
		expectNone(fmt.Sprintf("throttled unavailable evaluation %d", i))
	}
	spot(src, short)
	expectOne("tenth unavailable evaluation repeats", "ch-src-a", "10 unavailable evaluations")
	spot(src, errFix)
	expectOne("a changed reason repeats", "ch-src-a", "fetching BTC/USDT 4h candles failed")

	ts := StrategyConfig{ID: src.ID, Type: "futures", Platform: "topstep", Script: "shared_scripts/check_topstep.py",
		Args: []string{"breakout", "ES", "1h", "--mode=paper"}, HTFFilter: true, PaperSource: "src-a"}
	closedMarket := false
	htfITUseFixture(t, root, fixture(htfITFixture{HTF: "short", Spot: "up", Fut: "up", MarketOpen: &closedMarket}))
	if _, _, _, ok := runTopStepCheck(ts, map[string]float64{"ES": 100}, PositionCtx{}, nil, "simple", notifier, &StrategyLogger{stratID: ts.ID, writer: &logBuf}); ok {
		t.Fatal("a closed market returns no tradable result")
	}
	expectNone("not_evaluated")
	if htfITTrackerCount(src.ID) != 11 {
		t.Fatalf("not_evaluated must preserve the tracker without advancing, got %d", htfITTrackerCount(src.ID))
	}

	spot(src, neutral)
	expectOne("first successful evaluation", "ch-src-a", "**HTF FILTER RECOVERED** [htf-src] binanceus")
	spot(src, neutral)
	expectNone("second successful evaluation")

	for i := 0; i < 3; i++ {
		spot(src, short)
	}
	expectOne("alert before skipped", "ch-src-a", "3 unavailable evaluations")
	skipped := StrategyConfig{ID: src.ID, Type: "perps", Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"delta_neutral_funding", "BTC", "1h", "--mode=paper", "--inst-type=swap"}, HTFFilter: true, PaperSource: "src-a"}
	htfITUseFixture(t, root, fixture(short))
	if _, _, _, ok := runOKXCheck(skipped, map[string]float64{"BTC": 100}, PositionCtx{}, nil, "simple", notifier, &StrategyLogger{stratID: skipped.ID, writer: &logBuf}); !ok {
		t.Fatalf("an excluded open strategy must be a usable result\n%s", logBuf.String())
	}
	expectNone("skipped clears silently")
	if htfITTrackerCount(src.ID) != 0 {
		t.Fatalf("skipped must clear the tracker, got %d", htfITTrackerCount(src.ID))
	}
	spot(src, neutral)
	expectNone("ok after a silent clear has nothing to recover")

	for i := 1; i <= 14; i++ {
		spot(src, transient)
	}
	expectNone("transient upstream failures below the threshold")
	spot(src, transient)
	expectOne("fifteenth transient failure", "ch-src-a", "**HTF FILTER UNAVAILABLE (sustained upstream error)**")
	spot(src, neutral)
	expectOne("recovery after a transient alert", "ch-src-a", "after 15 unavailable evaluations")

	spot(src, transient)
	advance(76 * time.Minute)
	spot(src, transient)
	expectOne("transient failures past the duration limit", "ch-src-a", "2 unavailable evaluations")
	spot(src, neutral)
	sink.drain()

	for i := 0; i < 3; i++ {
		spot(live, short)
	}
	m, d := sink.drain()
	if len(m) != 3 || len(d) != 1 || !strings.Contains(d[0].content, "[live scope]") || !strings.Contains(d[0].content, "partition=live") {
		t.Fatalf("a live alert must reach every channel and the owner, got messages=%v dms=%v", m, d)
	}
	seen := map[string]bool{}
	for _, msg := range m {
		seen[msg.channelID] = true
	}
	if !seen["ch-live"] || !seen["ch-okx-paper"] || !seen["ch-src-a"] {
		t.Fatalf("live routing must keep the existing all-channel behavior, got %v", m)
	}

	cfg.Discord.Channels = map[string]string{"binanceus": "ch-live", "okx-paper": "ch-okx-paper", "binanceus-paper:src-a": "ch-src-b"}
	notifier.ReloadConfig(cfg)
	for i := 0; i < 3; i++ {
		spot(src, short)
	}
	expectOne("after a destination reload", "ch-src-b", "3 unavailable evaluations")
}

func htfITHLSnapshot(htfBars int, htfReady bool) (*marketSnapshot, float64) {
	sealedAt := time.Now().UTC()
	deadline := sealedAt.Truncate(time.Hour)
	snap := &marketSnapshot{
		Version:      marketSnapshotVersion,
		EvaluationID: fmt.Sprintf("htf-it/%d", sealedAt.UnixNano()),
		SealedAt:     sealedAt,
		keys:         make(map[marketFeedKey]*marketSnapshotKey),
		mids:         make(map[string]feedMid),
		funding:      make(map[string]feedFunding),
		observations: make(map[feedObservationKey]*marketSnapshotObservation),
	}
	mark := 0.0
	add := func(tf string, interval int64, total int, ready bool, spike bool) {
		key := feedKeyFor("BTC", tf)
		entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
		formingOpen := deadline.UnixMilli() - deadline.UnixMilli()%interval
		start := formingOpen - int64(total-1)*interval
		p := 100.0
		for i := 0; i < total; i++ {
			if i%2 == 0 {
				p += 0.4
			} else {
				p -= 0.35
			}
			open := start + int64(i)*interval
			b := feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: i < total-1, Open: p - 0.1, High: p + 0.5, Low: p - 0.5, Close: p, Volume: 1000 + float64(i), RecvAt: sealedAt}
			if spike && i == total-1 {
				b.High, b.Close = p+12.5, p+12
			}
			entry.Bars = append(entry.Bars, b)
		}
		status := feedStatusReady
		if !ready {
			status = feedStatusFailed
		}
		entry.Readiness = feedKeyReadiness{Key: key, Bars: total, Required: total, Ready: ready, Status: status, Source: "test", LastRecvAt: sealedAt}
		snap.keys[key] = entry
		if spike {
			mark = entry.Bars[total-1].Close
		}
	}
	add("1h", htfITHour, 260, true, true)
	add("4h", 4*htfITHour, htfBars, htfReady, false)
	snap.mids["BTC"] = feedMid{Px: mark, RecvAt: sealedAt, Source: "test"}
	return snap, mark
}

func htfITHLConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	strat := func(id string, closedBar bool) map[string]any {
		return map[string]any{
			"id": id, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []any{"breakout", "BTC", "1h", "--mode=paper"}, "capital": 1000, "leverage": 1, "max_drawdown_pct": 90,
			"htf_filter": true, "closed_bar_decisions": closedBar,
			"close_strategy": map[string]any{"name": "tiered_tp_pct"},
		}
	}
	blob, err := json.Marshal(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 3600,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "market_feed": "websocket",
		"strategies": []any{strat("hl-a", false), strat("hl-b", false), strat("hl-c", true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatalf("LoadConfigReadOnly: %v", err)
	}
	return cfg
}

func htfITHLStep(t *testing.T, cfg *Config, sc StrategyConfig, state *AppState, store *StateStore, mu *sync.RWMutex, prices map[string]float64, feed *marketFeedContext, batch *hlBatchCycleResults, logger *StrategyLogger) {
	t.Helper()
	s := state.Strategies[sc.ID]
	stratDB, err := store.dbForStrategy(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	part := partitionFor(sc)
	sr := &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(cfg, part)}
	mu.RLock()
	pos, hasPos := readHLPerpsPosition(sc, s, "BTC", cfg.Regime)
	cash := s.Cash
	mu.RUnlock()
	x := &hlPerpsStep{sc: sc, mu: mu, logger: logger, stratState: s, notifier: NewMultiNotifier(), cfg: cfg, stratDB: stratDB, store: store, state: state,
		sr: sr, prices: prices, feedCtx: feed, hlBatchResults: batch, intervals: map[string]int{sc.ID: 3600},
		stratScope: part.Scope, hlScaleInCash: cash}
	if hasPos {
		x.hlPosCtx, x.hlPosSide, x.hlPosQty, x.hlAvgCost = pos.Ctx, pos.Ctx.Side, pos.Ctx.Quantity, pos.Ctx.AvgCost
		x.hlPosLeverage, x.hlEntryATR, x.hlPosSnapshot = pos.Leverage, pos.EntryATR, pos.Snapshot
	}
	runHyperliquidPerpsStep(x)
}

func htfITSameBlock(a, b *HTFFilterEcho) bool {
	if a == nil || b == nil || a.Trend == nil || b.Trend == nil {
		return false
	}
	return a.Status == b.Status && a.Timeframe == b.Timeframe && *a.Trend == *b.Trend && a.Reason == b.Reason
}

func TestHTFFilterIntegrationHyperliquidFeedSingleAndBatch(t *testing.T) {
	root := htfITRoot(t)
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	resetHTFFilterTrackersForTest(t)
	t.Setenv(hlBatchDisabledEnv, "")
	fixture := filepath.Join(t.TempDir(), "offline.json")
	if err := os.WriteFile(fixture, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	htfITUseFixture(t, root, fixture)
	snap, mark := htfITHLSnapshot(49, true)
	cfg := htfITHLConfig(t)
	t.Chdir(root)
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	feed := &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
	prices := map[string]float64{"BTC": mark}
	byID := map[string]StrategyConfig{}
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var logBuf bytes.Buffer
	logger := func(id string) *StrategyLogger { return &StrategyLogger{stratID: id, writer: &logBuf} }

	single := byID["hl-a"]
	res, _, _, ok := runHyperliquidCheck(&single, prices, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), logger("hl-a"), nil, feed)
	if !ok || res == nil || res.HTFFilter == nil || res.HTFFilter.Status != htfFilterStatusUnavailable || !strings.Contains(res.HTFFilter.Reason, "49 4h candles available for BTC (need 50)") || res.Signal != 1 {
		t.Fatalf("a ready 49-bar HTF frame must reach the Python unavailable result with the open signal unfiltered: ok=%t res=%+v\n%s", ok, res, logBuf.String())
	}
	singleBlock := res.HTFFilter
	if htfITTrackerCount("hl-a") != 1 {
		t.Fatalf("a consumed single check must advance the HTF tracker once, got %d", htfITTrackerCount("hl-a"))
	}

	cb := byID["hl-c"]
	cbRes, _, _, ok := runHyperliquidCheck(&cb, prices, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), logger("hl-c"), nil, feed)
	if !ok || cbRes == nil || cbRes.ClosedBar == nil || !cbRes.ClosedBar.Held || cbRes.HTFFilter == nil || cbRes.HTFFilter.Status != htfFilterStatusUnavailable {
		t.Fatalf("a closed-bar HTF failure must keep its decision hold and report unavailable: ok=%t res=%+v\n%s", ok, cbRes, logBuf.String())
	}

	store := openSourceStore(t, cfg)
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	b := state.Strategies["hl-b"]
	b.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 5, InitialQuantity: 5, AvgCost: mark / 1.1, Side: "long", Leverage: 1, EntryATR: 1, OwnerStrategyID: "hl-b", OpenedAt: time.Now().Add(-2 * time.Hour)}
	b.Cash -= 5 * mark / 1.1
	mu := &sync.RWMutex{}
	batchStrats := []StrategyConfig{byID["hl-a"], byID["hl-b"]}
	batch := runHyperliquidBatchPrePass(batchStrats, state, mu, cfg, prices, NewMultiNotifier(), func(format string, a ...any) { fmt.Fprintf(&logBuf, format+"\n", a...) }, feed)
	outA, okA := batch.lookup("hl-a")
	outB, okB := batch.lookup("hl-b")
	if !okA || !okB || outA.Result == nil || outB.Result == nil {
		t.Fatalf("the batch must return both slots: %+v %+v\n%s", outA, outB, logBuf.String())
	}
	if !htfITSameBlock(outA.Result.HTFFilter, singleBlock) || !htfITSameBlock(outB.Result.HTFFilter, singleBlock) {
		t.Fatalf("batch slots and the single check must produce the same HTF result: single=%+v a=%+v b=%+v", singleBlock, outA.Result.HTFFilter, outB.Result.HTFFilter)
	}
	if htfITTrackerCount("hl-a") != 1 || htfITTrackerCount("hl-b") != 0 {
		t.Fatalf("the batch pre-pass must not advance HTF alert state: a=%d b=%d", htfITTrackerCount("hl-a"), htfITTrackerCount("hl-b"))
	}

	htfITHLStep(t, cfg, byID["hl-a"], state, store, mu, prices, feed, batch, logger("hl-a"))
	htfITHLStep(t, cfg, byID["hl-b"], state, store, mu, prices, feed, batch, logger("hl-b"))
	log := logBuf.String()
	if strings.Contains(log, "Batched check inputs changed") {
		t.Fatalf("the steps must consume the batch hits\n%s", log)
	}
	if htfITTrackerCount("hl-a") != 2 || htfITTrackerCount("hl-b") != 1 {
		t.Fatalf("consuming a batch hit must advance HTF alert state once: a=%d b=%d", htfITTrackerCount("hl-a"), htfITTrackerCount("hl-b"))
	}
	if !strings.Contains(log, "[hl-a] [WARN] HTF filter: BUY signal suppressed — higher-timeframe 4h input unavailable") {
		t.Fatalf("the flat Hyperliquid entry must be held\n%s", log)
	}
	mu.RLock()
	aPos := state.Strategies["hl-a"].Positions["BTC"]
	bPos := state.Strategies["hl-b"].Positions["BTC"]
	mu.RUnlock()
	if aPos != nil && aPos.Quantity > 0 {
		t.Fatalf("the held entry opened %+v", aPos)
	}
	if bPos != nil && bPos.Quantity > 0 {
		t.Fatalf("the tiered close must run while HTF input is unavailable, position %+v\n%s", bPos, log)
	}
	if strings.Contains(log, "network blocked by the htf offline fixture") {
		t.Fatalf("a sealed-feed check made a private fetch\n%s", log)
	}

	unready, _ := htfITHLSnapshot(60, false)
	unreadyFeed := &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: unready, Interval: cfg.IntervalSeconds}
	before := htfITTrackerCount("hl-a")
	logBuf.Reset()
	res, _, _, ok = runHyperliquidCheck(&single, prices, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), logger("hl-a"), nil, unreadyFeed)
	if !ok || res == nil || res.Degraded == "" || !strings.Contains(res.Degraded, "no ready higher-timeframe frame") {
		t.Fatalf("an unready HTF key must keep the existing degraded hold with protection: ok=%t res=%+v\n%s", ok, res, logBuf.String())
	}
	if htfITTrackerCount("hl-a") != before {
		t.Fatalf("a degraded result must not advance or clear HTF alert state")
	}
	if held, _ := feed.feedHoldsSignal(single); held {
		t.Fatal("a ready short frame is not a feed hold; the Python result owns the hold")
	}
	if held, why := unreadyFeed.feedHoldsSignal(single); !held || !strings.Contains(why, "higher-timeframe") {
		t.Fatalf("an unready HTF key keeps its feed hold, got %t %q", held, why)
	}
}
