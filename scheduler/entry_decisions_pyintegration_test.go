//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

const entryParityHourMs = int64(3_600_000)

var entryParityT0 = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).UnixMilli()

type entryParityBar struct{ O, H, L, C float64 }

func entryParityBars() []entryParityBar {
	out := make([]entryParityBar, 0, 64)
	for i := 0; i < 60; i++ {
		out = append(out, entryParityBar{100, 100.2, 99.8, 100})
	}
	return append(out,
		entryParityBar{100, 103, 99.9, 102.8},
		entryParityBar{102.8, 103, 99, 100.5},
		entryParityBar{100.5, 106, 100.4, 105.8},
		entryParityBar{105.8, 106.5, 105, 106},
	)
}

func entryParityOpenMs(i int) int64 { return entryParityT0 + int64(i)*entryParityHourMs }

func entryParityMarket(bars []entryParityBar, closedThrough int, cutoff int64) map[string]any {
	frame := bars[:closedThrough+2]
	rows := make([][]float64, 0, len(frame))
	timing := make([][]any, 0, len(frame))
	for i, b := range frame {
		open := entryParityOpenMs(i)
		closeMs := open + entryParityHourMs - 1
		rows = append(rows, []float64{float64(closeMs), b.O, b.H, b.L, b.C, 1000})
		timing = append(timing, []any{open, closeMs, true})
	}
	last := len(frame) - 1
	return map[string]any{
		"version": 3, "snapshot_id": "entry-parity/1", "generation": 1, "sealed_at_ms": cutoff, "decision_cutoff_ms": cutoff,
		"feed_complete": true,
		"frames": map[string]any{"BTC|1h": map[string]any{
			"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": false,
			"first_open_ms": entryParityOpenMs(0), "last_open_ms": entryParityOpenMs(last),
			"last_close_ms": entryParityOpenMs(last) + entryParityHourMs - 1, "last_recv_at_ms": cutoff,
			"source": "ws", "ready": true, "forming_bar_included": true,
			"timing": map[string]any{"rule": "hyperliquid_native_close", "interval_ms": entryParityHourMs, "bars": timing},
		}},
		"mids": map[string]any{"BTC": map[string]any{"px": frame[last].C, "recv_at_ms": cutoff, "source": "ws",
			"age_ms": 0, "stale": false, "confirmed": true}},
	}
}

func entryParityCheck(t *testing.T, python, root string, bars []entryParityBar, closedThrough int, cutoff int64, posSide string, posQty float64) *HyperliquidResult {
	t.Helper()
	argv := []string{filepath.Join(root, "shared_scripts", "check_hyperliquid.py"), "breakout", "BTC", "1h", "--mode=paper",
		"--market-stdin", "--ohlcv-limit", "200", closedBarDecisionsFlag}
	if posQty > 0 {
		argv = append(argv, "--position-side", posSide, "--position-avg-cost=100", "--position-qty="+formatEntryFloat(posQty), "--position-initial-qty="+formatEntryFloat(posQty), "--position-entry-atr=1")
	}
	stdin, err := json.Marshal(map[string]any{"v": 2, "market": entryParityMarket(bars, closedThrough, cutoff)})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, argv...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HYPERLIQUID_SECRET_KEY=", "HYPERLIQUID_ACCOUNT_ADDRESS=", "OKX_API_KEY=", "GO_TRADER_HL_OHLCV_CACHE=0")
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("check failed: %v\nstdout=%s\nstderr=%s", err, out, stderr.String())
	}
	var res HyperliquidResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("parse check output: %v\n%s", err, out)
	}
	if res.Error != "" {
		t.Fatalf("check error: %s", res.Error)
	}
	if msg := closedBarDecisionContractError(StrategyConfig{ClosedBarDecisions: true}, res.StrategyDecisionFields); msg != "" {
		t.Fatalf("closed-bar contract: %s", msg)
	}
	return &res
}

func formatEntryFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const entryParityDriver = `import json
import os
import sys

root = sys.argv[1]
for sub in ("backtest", "shared_tools"):
    sys.path.insert(0, os.path.join(root, sub))

import pandas as pd
from backtester import Backtester

req = json.load(sys.stdin)
bars = req["bars"]
idx = pd.to_datetime([b[0] for b in bars], unit="ms")
df = pd.DataFrame({
    "open": [b[1] for b in bars],
    "high": [b[2] for b in bars],
    "low": [b[3] for b in bars],
    "close": [b[4] for b in bars],
    "volume": [1000.0] * len(bars),
}, index=idx)
df["open_action"] = req["open_action"]
df["close_fraction"] = [0.0] * len(bars)
bt = Backtester(initial_capital=1000.0, platform="hyperliquid", stop_loss_pct=req["stop_loss_pct"],
                direction="long", strategy_type="perps")
res = bt.run(df, strategy_name="entry-parity", symbol="BTC/USDT", timeframe="1h", save=False)
entries = []
for t in res["trades"]:
    entries.append(int(pd.Timestamp(t["entry_date"]).value // 1_000_000))
print(json.dumps({"entries": entries, "exit_reasons": [t.get("exit_reason") for t in res["trades"]]}))
`

func runEntryParityBacktest(t *testing.T, python, root string, bars []entryParityBar, openAction []string, stopLossPct float64) []int64 {
	t.Helper()
	dir := t.TempDir()
	driver := filepath.Join(dir, "entry_parity_driver.py")
	if err := os.WriteFile(driver, []byte(entryParityDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := make([][]float64, 0, len(bars))
	for i, b := range bars {
		rows = append(rows, []float64{float64(entryParityOpenMs(i)), b.O, b.H, b.L, b.C})
	}
	body, err := json.Marshal(map[string]any{"bars": rows, "open_action": openAction, "stop_loss_pct": stopLossPct})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", driver, root)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("backtest driver: %v\n%s", err, stderr.String())
	}
	var out struct {
		Entries     []int64  `json:"entries"`
		ExitReasons []string `json:"exit_reasons"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse driver output: %v\n%s", err, stdout.String())
	}
	decisions := make([]int64, 0, len(out.Entries))
	for _, e := range out.Entries {
		decisions = append(decisions, e-entryParityHourMs)
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i] < decisions[j] })
	return decisions
}

func TestClosedBarEntryAllowanceMatchesBacktesterWithRealCheckScript(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	bars := entryParityBars()

	for _, cadence := range []time.Duration{600 * time.Second, 90 * time.Second} {
		t.Run(cadence.String(), func(t *testing.T) {
			sc := entryTestHLStrategy("hl-parity", "paper")
			sc.Direction = DirectionLong
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			signals := map[int]int{}
			var runtimeEntries []int64
			stoppedOut := false
			heldRepeats := 0
			for closedThrough := 60; closedThrough <= 62; closedThrough++ {
				formingOpen := entryParityOpenMs(closedThrough + 1)
				for cut := formingOpen + 10_000; cut < formingOpen+entryParityHourMs; cut += cadence.Milliseconds() {
					qty, side := env.position(sc.ID, "BTC")
					res := entryParityCheck(t, python, root, bars, closedThrough, cut, side, qty)
					if res.ClosedBar.BarOpenMs != entryParityOpenMs(closedThrough) || res.ClosedBar.Held {
						t.Fatalf("check at closed bar %d decided on %d (held=%t %s)", closedThrough, res.ClosedBar.BarOpenMs, res.ClosedBar.Held, res.ClosedBar.HoldReason)
					}
					if prev, ok := signals[closedThrough]; ok && prev != res.Signal {
						t.Fatalf("repeated checks of closed bar %d disagree: %d then %d", closedThrough, prev, res.Signal)
					}
					signals[closedThrough] = res.Signal
					now := time.UnixMilli(cut + 1000)
					adm, trades := env.dispatchHLPaper(sc, res.Signal, res.Price, res.StrategyDecisionFields, now)
					if adm.admitted() && trades > 0 {
						runtimeEntries = append(runtimeEntries, adm.BarOpenMs)
					}
					if closedThrough == 60 && stoppedOut {
						if !adm.Held || trades != 0 {
							t.Fatalf("repeated check of the consumed candle at %s: held=%t trades=%d", time.UnixMilli(cut).UTC().Format(time.RFC3339), adm.Held, trades)
						}
						heldRepeats++
					}
					if closedThrough == 60 && !stoppedOut {
						if q, _ := env.position(sc.ID, "BTC"); q <= 0 {
							t.Fatalf("the first check of the decision candle did not open (signal %d, %q)", res.Signal, adm.HoldReason)
						}
						env.stopOut(sc.ID, "BTC", 100.7)
						stoppedOut = true
					}
				}
				env.saveAll()
				env = env.restart()
			}
			if want := int(time.Hour/cadence) - 1; heldRepeats < want {
				t.Fatalf("held %d repeated checks of the consumed candle, want at least %d", heldRepeats, want)
			}
			if len(runtimeEntries) != 2 {
				t.Fatalf("runtime entries %v, want two decision candles", runtimeEntries)
			}
			if signals[60] != 1 || signals[62] != 1 || signals[61] != 0 {
				t.Fatalf("fixture signals %v, want long on closed bars 60 and 62 only", signals)
			}
			openAction := make([]string, len(bars))
			for i := range openAction {
				openAction[i] = "none"
				if signals[i] == 1 {
					openAction[i] = "long"
				}
			}
			backtest := runEntryParityBacktest(t, python, root, bars, openAction, 0.02)
			if !reflect.DeepEqual(runtimeEntries, backtest) {
				t.Fatalf("entries per decision candle: runtime %v, backtester %v", runtimeEntries, backtest)
			}
			if got := env.consumedBar(sc.ID, "BTC", "long"); got != entryParityOpenMs(62) {
				t.Fatalf("consumed bar %d, want %d", got, entryParityOpenMs(62))
			}
		})
	}
}
