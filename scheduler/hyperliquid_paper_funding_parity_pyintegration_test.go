//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const hlPaperFundingParityDriver = `import json
import os
import sys

root = sys.argv[1]
for sub in ("backtest", "shared_tools"):
    sys.path.insert(0, os.path.join(root, sub))

import numpy as np
import pandas as pd
from backtester import Backtester
from funding_fetcher import attach_funding_accrual_column

req = json.load(sys.stdin)
closes = [float(c) for c in req["closes"]]
n = len(closes)
idx = pd.date_range(pd.Timestamp(req["start_ms"], unit="ms", tz="UTC"), periods=n, freq="1h")
df = pd.DataFrame({"open": closes, "high": closes, "low": closes, "close": closes, "volume": np.ones(n)}, index=idx)
df["open_action"] = req["open_action"]
df["close_fraction"] = [float(f) for f in req["close_fraction"]]
funding = pd.DataFrame({"timestamp": [int(t.value // 1_000_000) for t in idx], "rate": [float(r) for r in req["rates"]]})
df = attach_funding_accrual_column(df, funding)
spec = {"taker_fee_pct": 0.0, "maker_fee_pct": 0.0, "half_spread_pct": 0.0, "slippage_pct": 0.0,
        "size_decimals": 4, "min_notional_usd": 0.0, "min_notional_margin": 0.0}
bt = Backtester(initial_capital=10000, platform="hyperliquid", execution_spec=spec, direction="both")
res = bt.run(df, strategy_name="funding-parity", symbol="ETH/USDT", timeframe="1h", save=False, record_events=True)
out = {"events": [], "total_funding": res["total_funding_pnl"]}
for e in res["ledger_events"]["events"]:
    if e["kind"] not in ("open", "close", "funding"):
        continue
    out["events"].append({
        "kind": e["kind"],
        "ms": int(pd.Timestamp(e["bar_timestamp"]).value // 1_000_000),
        "qty_before": e["qty_before"],
        "qty_after": e["qty_after"],
        "price": e["raw_price"],
        "funding_cash": e["funding_cash"],
    })
json.dump(out, sys.stdout)
`

type hlPaperFundingParityEvent struct {
	Kind        string   `json:"kind"`
	Ms          int64    `json:"ms"`
	QtyBefore   float64  `json:"qty_before"`
	QtyAfter    float64  `json:"qty_after"`
	Price       float64  `json:"price"`
	FundingCash *float64 `json:"funding_cash"`
}

type hlPaperFundingParityOutput struct {
	Events       []hlPaperFundingParityEvent `json:"events"`
	TotalFunding float64                     `json:"total_funding"`
}

func runHLPaperFundingParityDriver(t *testing.T, req map[string]any) hlPaperFundingParityOutput {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "funding_parity_driver.py")
	if err := os.WriteFile(driver, []byte(hlPaperFundingParityDriver), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", driver, root)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("driver: %v\n%s", err, stderr.String())
	}
	var out hlPaperFundingParityOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("driver output: %v\n%s", err, stdout.String())
	}
	return out
}

func TestHLPaperFundingParityWithBacktester(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closes func(i int) float64
	}{
		{"constant mark", func(int) float64 { return 2000 }},
		{"varying mark sampled at each bar close", func(i int) float64 { return 2000 + 10*float64(i) + 3*float64(i%3) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 16
			closes := make([]float64, n)
			openAction := make([]string, n)
			closeFraction := make([]float64, n)
			rates := make([]float64, n)
			for i := 0; i < n; i++ {
				closes[i] = tc.closes(i)
				openAction[i] = "none"
				rates[i] = 0.0001 * float64((i%5)-2)
			}
			openAction[2], closeFraction[6] = "long", 1
			openAction[8], closeFraction[12] = "short", 1
			out := runHLPaperFundingParityDriver(t, map[string]any{
				"start_ms": pfT0, "closes": closes, "open_action": openAction, "close_fraction": closeFraction, "rates": rates,
			})
			simFunding := map[int64]float64{}
			for _, e := range out.Events {
				if e.Kind == "funding" && e.FundingCash != nil {
					simFunding[e.Ms] = *e.FundingCash
				}
			}
			if len(simFunding) == 0 {
				t.Fatal("the simulator booked no funding, so the comparison proves nothing")
			}

			f := newPFFixture(t, true, pfPaperPerps("hl-par", "ETH"))
			f.at(pfT0 - 30*60_000)
			f.sync()
			s := f.state.Strategies["hl-par"]
			var records []feedFundingRecord
			for k := 0; k < n; k++ {
				tk := pfT0 + int64(k)*pfHour
				records = append(records, feedFundingRecord{TimeMs: tk, Rate: rates[k]})
				for _, e := range out.Events {
					if e.Ms != tk {
						continue
					}
					f.at(tk)
					switch e.Kind {
					case "open":
						signal := 1
						if e.QtyAfter < 0 {
							signal = -1
						}
						pfOpen(t, s, "ETH", signal, math.Abs(e.QtyAfter), e.Price)
					case "close":
						if !bookPerpsCloseWithFillFee(s, "ETH", e.Price, 0, false, "", "parity", "parity", "parity", nil) {
							t.Fatalf("close at %d did not book", tk)
						}
					}
				}
				f.run(tk+59*60_000, map[string]float64{"ETH": closes[k]}, pfCoverage("ETH", pfT0-pfHour, records...))
			}
			goFunding := map[int64]float64{}
			total := 0.0
			for _, tr := range pfFundingRows(s) {
				goFunding[tr.Timestamp.UnixMilli()] = tr.RealizedPnL
				total += tr.RealizedPnL
			}
			if len(goFunding) != len(simFunding) {
				t.Fatalf("Go booked %d funding events, the simulator %d:\n go %v\nsim %v", len(goFunding), len(simFunding), goFunding, simFunding)
			}
			simTotal := 0.0
			for ms, cash := range simFunding {
				simTotal += cash
				if got, ok := goFunding[ms]; !ok || math.Abs(got-cash) > 1e-9 {
					t.Fatalf("funding at %s: Go %v, simulator %v", time.UnixMilli(ms).UTC().Format(time.RFC3339), got, cash)
				}
			}
			if math.Abs(total-simTotal) > 1e-9 || math.Abs(simTotal-out.TotalFunding) > 1e-3 {
				t.Fatalf("total funding cash: Go %v, simulator events %v, simulator summary %v", total, simTotal, out.TotalFunding)
			}
		})
	}
}
