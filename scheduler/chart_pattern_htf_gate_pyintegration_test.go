//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const chartPatternITID = "bus-cpat-btc"

func chartPatternITLinspace(a, b float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = a + (b-a)*float64(i)/float64(n-1)
	}
	return out
}

func chartPatternITCloses(n int, kind string) []float64 {
	var tail []float64
	if kind == "buy" {
		tail = append(tail, chartPatternITLinspace(100, 80, 20)...)
		tail = append(tail, chartPatternITLinspace(80, 90, 15)[1:]...)
		tail = append(tail, chartPatternITLinspace(90, 81, 15)[1:]...)
		tail = append(tail, chartPatternITLinspace(81, 89.5, 12)[1:]...)
		tail = append(tail, 91.0)
	} else {
		tail = append(tail, chartPatternITLinspace(80, 100, 20)...)
		tail = append(tail, chartPatternITLinspace(100, 90, 15)[1:]...)
		tail = append(tail, chartPatternITLinspace(90, 99, 15)[1:]...)
		tail = append(tail, chartPatternITLinspace(99, 90.5, 12)[1:]...)
		tail = append(tail, 89.0)
	}
	return append(chartPatternITLinspace(40, tail[0]-0.5, n-len(tail)), tail...)
}

func chartPatternITWriteFixture(t *testing.T, path string, n int, kind string) {
	t.Helper()
	closes := chartPatternITCloses(n, kind)
	end := htfITNow()
	rows := make([][]any, 0, n)
	for i, c := range closes {
		vol := 100.0
		if i == n-1 {
			vol = 1000.0
		}
		rows = append(rows, []any{end - int64(n-1-i)*htfITHour, c, c + 0.2, c - 0.2, c, vol})
	}
	blob, err := json.Marshal(map[string]any{
		"ccxt":    map[string]any{"BTC/USDT|1h": rows},
		"tickers": map[string]any{"BTC/USDT": closes[n-1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func chartPatternITConfig(t *testing.T, dir string, factor int) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"),
		"strategies": []any{map[string]any{
			"id": chartPatternITID, "type": "spot", "platform": "binanceus", "script": "shared_scripts/check_strategy.py",
			"args": []string{chartPatternStrategyName, "BTC/USDT", "1h"}, "capital": 1000, "max_drawdown_pct": 90, "interval_seconds": 60,
			"open_strategy": map[string]any{"name": chartPatternStrategyName, "params": map[string]any{"htf_gate_factor": factor}},
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func chartPatternITEnv(root, fixture string) []string {
	return append(os.Environ(),
		"PYTHONPATH="+filepath.Join(root, "shared_scripts", "testdata", "htf_offline"),
		"HTF_OFFLINE_FIXTURE="+fixture,
		"DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
}

func TestChartPatternHTFGateRuntimeShortageHoldsEntriesAndKeepsCloses(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	cfg := chartPatternITConfig(t, dir, 4)
	port := strconv.Itoa(htfITFreePort(t))
	run := func(name string, n int, kind string) string {
		t.Helper()
		fixture := filepath.Join(dir, name+".json")
		chartPatternITWriteFixture(t, fixture, n, kind)
		cmd := exec.Command(bin, "--config", cfg, "--once", "--status-port", port)
		cmd.Dir = root
		cmd.Env = chartPatternITEnv(root, fixture)
		out, err := cmd.CombinedOutput()
		log := string(out)
		if err != nil {
			t.Fatalf("%s: scheduler --once failed: %v\n%s", name, err, log)
		}
		if strings.Contains(log, "network blocked by the htf offline fixture") {
			t.Fatalf("%s: a check tried an uncontrolled network call\n%s", name, log)
		}
		t.Logf("%s log:\n%s", name, log)
		return log
	}
	cold := "[" + chartPatternITID + "] [WARN] Chart pattern HTF gate cold: htf_gate_factor=4 (htf_gate_ema_slow=40, htf_gate_mode=\"veto\") got 150 candles ("
	needs := "needs 164 candles (41 buckets)"
	held := "[" + chartPatternITID + "] [WARN] Chart pattern HTF gate: BUY signal suppressed"

	flat := run("cold-buy", 150, "buy")
	if !strings.Contains(flat, cold) || !strings.Contains(flat, needs) {
		t.Fatalf("a 150-candle frame at factor 4 must log the cold gate with the returned and needed candles\n%s", flat)
	}
	if !strings.Contains(flat, held) {
		t.Fatalf("the cold gate must hold the flat BUY entry\n%s", flat)
	}
	if n := htfITPositions(t, flat, chartPatternITID); n != 0 {
		t.Fatalf("the held entry opened %d position(s)", n)
	}

	warm := run("warm-buy", 200, "buy")
	if strings.Contains(warm, "Chart pattern HTF gate") {
		t.Fatalf("a warm 200-candle frame must not warn or hold\n%s", warm)
	}
	if n := htfITPositions(t, warm, chartPatternITID); n != 1 {
		t.Fatalf("a warm gate with an up trend must pass the BUY, got %d position(s)", n)
	}

	closes := run("cold-sell", 150, "sell")
	if !strings.Contains(closes, cold) {
		t.Fatalf("the cold gate must warn on the close cycle too\n%s", closes)
	}
	if strings.Contains(closes, "signal suppressed") {
		t.Fatalf("a reducing SELL must pass the cold gate\n%s", closes)
	}
	if !strings.Contains(closes, "["+chartPatternITID+"] [INFO] SELL ") {
		t.Fatalf("the close must execute while the gate is cold\n%s", closes)
	}
	if n := htfITPositions(t, closes, chartPatternITID); n != 0 {
		t.Fatalf("the close must flatten the position, got %d", n)
	}
}

func TestChartPatternHTFGateRefusedReloadKeepsThePreviousConfig(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	chartPatternITWriteFixture(t, fixture, 200, "sell")
	cfg := chartPatternITConfig(t, dir, 4)
	good, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config", cfg, "--status-port", strconv.Itoa(htfITFreePort(t)))
	cmd.Dir = root
	cmd.Env = chartPatternITEnv(root, fixture)
	log := &robinhoodAdmissionDaemon{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			cmd.Process.Kill()
			<-exited
		}
	})
	waitFor := func(what string, cond func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(150 * time.Second)
		for time.Now().Before(deadline) {
			if cond(log.String()) {
				return
			}
			select {
			case <-exited:
				t.Fatalf("daemon exited while waiting for %s:\n%s", what, log.String())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatalf("timed out waiting for %s:\n%s", what, log.String())
	}
	checked := "| 1 strategies checked |"
	waitFor("a first cycle", func(s string) bool { return strings.Contains(s, checked) })
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	doc["strategies"].([]any)[0].(map[string]any)["open_strategy"] = map[string]any{"name": chartPatternStrategyName, "params": map[string]any{"htf_gate_factor": 5, "htf_gate_mode": "align"}}
	bad, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor("the refused reload", func(s string) bool {
		return strings.Contains(s, "[reload] ERROR: reload failed; keeping previous config: config validation errors:") &&
			strings.Contains(s, "strategy "+chartPatternITID+": open strategy chart_pattern htf_gate_factor=5 (htf_gate_mode=\"align\", registry default htf_gate_ema_slow=40) cannot warm in the default check frame of 200 candles")
	})
	before := strings.Count(log.String(), checked)
	waitFor("a cycle after the refused reload", func(s string) bool { return strings.Count(s, checked) > before })
	if strings.Contains(log.String(), "Config reload applied") {
		t.Fatalf("the refused reload applied a change:\n%s", log.String())
	}
	cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("daemon did not drain:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "[shutdown] Complete.") {
		t.Fatalf("daemon did not shut down cleanly after the refused reload:\n%s", log.String())
	}
}

const chartPatternITFuturesID = "ts-cpat-es"

func chartPatternITWriteFuturesFixture(t *testing.T, path string, n int, kind string) {
	t.Helper()
	closes := chartPatternITCloses(n, kind)
	end := htfITNow()
	rows := make([][]any, 0, n)
	for i, c := range closes {
		vol := 100.0
		if i == n-1 {
			vol = 1000.0
		}
		rows = append(rows, []any{end - int64(n-1-i)*htfITHour, c, c + 0.2, c - 0.2, c, vol})
	}
	blob, err := json.Marshal(map[string]any{
		"yahoo":               map[string]any{"ES=F|1h": rows},
		"yahoo_price":         map[string]any{"ES=F": closes[n-1]},
		"topstep_market_open": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChartPatternHTFGateColdReversalClosesWithoutOpeningTheOtherSide(t *testing.T) {
	root := htfITRoot(t)
	bin := pyintegrationBinary(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	blob, err := json.MarshalIndent(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"),
		"strategies": []any{map[string]any{
			"id": chartPatternITFuturesID, "type": "futures", "platform": "topstep", "script": "shared_scripts/check_topstep.py",
			"args": []string{chartPatternStrategyName, "ES", "1h", "--mode=paper"}, "capital": 50000, "max_drawdown_pct": 90, "interval_seconds": 60,
			"open_strategy": map[string]any{"params": map[string]any{"htf_gate_factor": 4}},
		}},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(htfITFreePort(t))
	run := func(name string, n int, kind string) string {
		t.Helper()
		fixture := filepath.Join(dir, name+".json")
		chartPatternITWriteFuturesFixture(t, fixture, n, kind)
		cmd := exec.Command(bin, "--config", cfg, "--once", "--status-port", port)
		cmd.Dir = root
		cmd.Env = chartPatternITEnv(root, fixture)
		out, err := cmd.CombinedOutput()
		log := string(out)
		if err != nil {
			t.Fatalf("%s: scheduler --once failed: %v\n%s", name, err, log)
		}
		if strings.Contains(log, "network blocked by the htf offline fixture") {
			t.Fatalf("%s: a check tried an uncontrolled network call\n%s", name, log)
		}
		t.Logf("%s log:\n%s", name, log)
		return log
	}
	tag := "[" + chartPatternITFuturesID + "] "

	opened := run("warm-buy", 200, "buy")
	if strings.Contains(opened, "Chart pattern HTF gate") {
		t.Fatalf("a warm 200-candle frame must not warn or hold\n%s", opened)
	}
	if n := htfITPositions(t, opened, chartPatternITFuturesID); n != 1 {
		t.Fatalf("a warm gate with an up trend must open the long, got %d position(s)", n)
	}

	added := run("cold-buy-long", 150, "buy")
	if !strings.Contains(added, tag+"[WARN] Chart pattern HTF gate: BUY signal suppressed — the higher-timeframe trend is cold this cycle") {
		t.Fatalf("a BUY addition to the long must be held while the gate is cold\n%s", added)
	}
	if strings.Contains(added, "reversal closes") {
		t.Fatalf("a BUY addition is no reversal\n%s", added)
	}
	if n := htfITPositions(t, added, chartPatternITFuturesID); n != 1 {
		t.Fatalf("the held addition must keep the one long, got %d position(s)", n)
	}

	reversed := run("cold-sell-long", 150, "sell")
	if !strings.Contains(reversed, tag+"[WARN] Chart pattern HTF gate: SELL reversal closes the open long position and opens no new side — the higher-timeframe trend is cold this cycle") {
		t.Fatalf("a SELL reversal on a long must close it while the gate is cold\n%s", reversed)
	}
	if strings.Contains(reversed, "signal suppressed") {
		t.Fatalf("the reversal exit must not be dropped\n%s", reversed)
	}
	if !strings.Contains(reversed, tag+"[INFO] SELL ") || strings.Contains(reversed, "Open short") || strings.Contains(reversed, "SHORT") {
		t.Fatalf("the reversal must close the long and open no short\n%s", reversed)
	}
	if n := htfITPositions(t, reversed, chartPatternITFuturesID); n != 0 {
		t.Fatalf("the reversal must leave the slot flat, got %d position(s)", n)
	}

	flat := run("cold-sell-flat", 150, "sell")
	if !strings.Contains(flat, tag+"[WARN] Chart pattern HTF gate: SELL signal suppressed — the higher-timeframe trend is cold this cycle") {
		t.Fatalf("a flat SELL entry must be held while the gate is cold\n%s", flat)
	}
	if n := htfITPositions(t, flat, chartPatternITFuturesID); n != 0 {
		t.Fatalf("the held short entry opened %d position(s)", n)
	}
}
