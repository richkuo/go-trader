//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	t.Parallel()
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

const chartPatternITHLID = "hl-cpat-btc"

func chartPatternITHLSnapshot(n, required int, kind string, mark float64) *marketSnapshot {
	sealedAt := time.Now().UTC()
	deadline := sealedAt.Truncate(time.Hour)
	snap := &marketSnapshot{
		Version:      marketSnapshotVersion,
		EvaluationID: fmt.Sprintf("cpat-it/%d", sealedAt.UnixNano()),
		SealedAt:     sealedAt,
		keys:         make(map[marketFeedKey]*marketSnapshotKey),
		mids:         make(map[string]feedMid),
		funding:      make(map[string]feedFunding),
		observations: make(map[feedObservationKey]*marketSnapshotObservation),
	}
	key := feedKeyFor("BTC", "1h")
	interval := int64(htfITHour)
	entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
	formingOpen := deadline.UnixMilli() - deadline.UnixMilli()%interval
	start := formingOpen - int64(n-1)*interval
	for i, c := range chartPatternITCloses(n, kind) {
		vol := 100.0
		if i == n-1 {
			vol = 1000.0
		}
		open := start + int64(i)*interval
		entry.Bars = append(entry.Bars, feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: i < n-1, Open: c, High: c + 0.2, Low: c - 0.2, Close: c, Volume: vol, RecvAt: sealedAt})
	}
	entry.Readiness = feedKeyReadiness{Key: key, Bars: n, Required: max(n, required), CoverageShort: n < required, Ready: true, Status: feedStatusReady, Source: "test", LastRecvAt: sealedAt}
	snap.keys[key] = entry
	snap.mids["BTC"] = feedMid{Px: mark, RecvAt: sealedAt, Source: "test"}
	return snap
}

func TestChartPatternHTFGateColdReversalClosesWithoutOpeningTheOtherSide(t *testing.T) {
	root := htfITRoot(t)
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	t.Setenv(hlBatchDisabledEnv, "")
	fixture := filepath.Join(t.TempDir(), "offline.json")
	if err := os.WriteFile(fixture, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	htfITUseFixture(t, root, fixture)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	blob, err := json.Marshal(map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 3600,
		"db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"), "market_feed": "websocket",
		"strategies": []any{map[string]any{
			"id": chartPatternITHLID, "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
			"args": []any{chartPatternStrategyName, "BTC", "1h", "--mode=paper"}, "capital": 1000, "leverage": 1, "max_drawdown_pct": 90,
			"direction": DirectionBoth, "open_strategy": map[string]any{"params": map[string]any{"htf_gate_factor": 4}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigReadOnly(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfigReadOnly: %v", err)
	}
	sc := cfg.Strategies[0]
	if !PerpsAllowsShort(sc) {
		t.Fatal("the strategy must allow shorts so a reversal is a two-sided signal")
	}
	t.Chdir(root)
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := openSourceStore(t, cfg)
	state := NewAppState()
	state.Strategies[sc.ID] = NewStrategyState(sc)
	mu := &sync.RWMutex{}
	var logBuf bytes.Buffer
	logger := &StrategyLogger{stratID: sc.ID, writer: &logBuf}
	const mark = 91.0
	run := func(name string, n int, kind string) string {
		t.Helper()
		logBuf.Reset()
		feed := &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: chartPatternITHLSnapshot(n, feedSignalLookback(cfg.Regime), kind, mark), Interval: cfg.IntervalSeconds}
		htfITHLStep(t, cfg, sc, state, store, mu, map[string]float64{"BTC": mark}, feed, nil, logger)
		log := logBuf.String()
		if strings.Contains(log, "network blocked by the htf offline fixture") {
			t.Fatalf("%s: a sealed-feed check made a private fetch\n%s", name, log)
		}
		t.Logf("%s log:\n%s", name, log)
		return log
	}
	position := func() *Position {
		mu.RLock()
		defer mu.RUnlock()
		if p := state.Strategies[sc.ID].Positions["BTC"]; p != nil && p.Quantity > 0 {
			cp := *p
			return &cp
		}
		return nil
	}
	tag := "[" + chartPatternITHLID + "] "
	reversal := func(signal, side string) string {
		return tag + "[WARN] Chart pattern HTF gate: " + signal + " reversal closes the open " + side + " position and opens no new side — the higher-timeframe trend is cold this cycle"
	}
	suppressed := func(signal string) string {
		return tag + "[WARN] Chart pattern HTF gate: " + signal + " signal suppressed — the higher-timeframe trend is cold this cycle"
	}

	opened := run("warm-buy", 200, "buy")
	if strings.Contains(opened, "Chart pattern HTF gate") {
		t.Fatalf("a warm 200-candle frame must not warn or hold\n%s", opened)
	}
	long := position()
	if long == nil || long.Side != "long" {
		t.Fatalf("a warm gate with an up trend must open the long, got %+v\n%s", long, opened)
	}

	added := run("cold-buy-long", 150, "buy")
	if !strings.Contains(added, suppressed("BUY")) || strings.Contains(added, "reversal closes") {
		t.Fatalf("a BUY addition to the long must be held while the gate is cold\n%s", added)
	}
	if p := position(); p == nil || p.Side != "long" || p.Quantity != long.Quantity {
		t.Fatalf("the held addition must keep the one long unchanged, got %+v want %+v", p, long)
	}

	reversed := run("cold-sell-long", 150, "sell")
	if !strings.Contains(reversed, reversal("SELL", "long")) || strings.Contains(reversed, "signal suppressed") {
		t.Fatalf("a SELL reversal on a long must close it while the gate is cold\n%s", reversed)
	}
	if p := position(); p != nil {
		t.Fatalf("the reversal must close the long and open no short, got %+v\n%s", p, reversed)
	}

	flatSell := run("cold-sell-flat", 150, "sell")
	if !strings.Contains(flatSell, suppressed("SELL")) {
		t.Fatalf("a flat SELL entry must be held while the gate is cold\n%s", flatSell)
	}
	if p := position(); p != nil {
		t.Fatalf("the held short entry opened %+v", p)
	}

	mu.Lock()
	s := state.Strategies[sc.ID]
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 5, InitialQuantity: 5, AvgCost: mark, Side: "short", Leverage: 1, EntryATR: 1, OwnerStrategyID: sc.ID, OpenedAt: time.Now().Add(-2 * time.Hour)}
	s.Cash -= 5 * mark
	mu.Unlock()
	coveredShort := run("cold-buy-short", 150, "buy")
	if !strings.Contains(coveredShort, reversal("BUY", "short")) || strings.Contains(coveredShort, "signal suppressed") {
		t.Fatalf("a BUY reversal on a short must close it while the gate is cold\n%s", coveredShort)
	}
	if p := position(); p != nil {
		t.Fatalf("the reversal must close the short and open no long, got %+v\n%s", p, coveredShort)
	}
}
