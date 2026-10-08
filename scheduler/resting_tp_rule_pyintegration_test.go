//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	restingTPPyHourMs = int64(3_600_000)
	restingTPPyT0     = int64(1_699_999_200_000)
)

type restingTPPyBar struct{ O, H, L, C float64 }

type restingTPPyStep struct {
	closedThrough  int
	stop           float64
	stopAfterCheck float64
	wantSent       float64
	wantHeld       bool
	wantFraction   float64
	wantFillPx     float64
	wantStopBar    int
	wantRecorded   bool
	wantWatermark  int
	wantReach      bool
	wantStoredStop float64
}

type restingTPPyScenario struct {
	name   string
	side   string
	tail   []restingTPPyBar
	tiers  []map[string]float64
	steps  []restingTPPyStep
	preset func(*Position)
}

func restingTPPyBars(tail []restingTPPyBar) [][6]float64 {
	out := make([][6]float64, 0, 60+len(tail))
	for i := 0; i < 60; i++ {
		out = append(out, [6]float64{100.0, 100.2, 99.8, 100.0})
	}
	for _, b := range tail {
		out = append(out, [6]float64{b.O, b.H, b.L, b.C})
	}
	return out
}

func restingTPPyOpenMs(i int) int64 { return restingTPPyT0 + int64(i)*restingTPPyHourMs }

func restingTPPyMarket(bars [][6]float64, closedThrough int) map[string]any {
	frame := bars[:closedThrough+2]
	rows := make([][]float64, 0, len(frame))
	timing := make([][]any, 0, len(frame))
	for i, b := range frame {
		open := restingTPPyOpenMs(i)
		closeMs := open + restingTPPyHourMs - 1
		rows = append(rows, []float64{float64(closeMs), b[0], b[1], b[2], b[3], 1000.0})
		timing = append(timing, []any{open, closeMs, true})
	}
	last := len(frame) - 1
	cutoff := restingTPPyOpenMs(last) + restingTPPyHourMs/2
	return map[string]any{
		"version": 1, "snapshot_id": "resting/1", "generation": 1, "sealed_at_ms": cutoff, "decision_cutoff_ms": cutoff,
		"feed_complete": true,
		"frames": map[string]any{"BTC|1h": map[string]any{
			"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": false,
			"first_open_ms": restingTPPyOpenMs(0), "last_open_ms": restingTPPyOpenMs(last),
			"last_close_ms": restingTPPyOpenMs(last) + restingTPPyHourMs - 1, "last_recv_at_ms": restingTPPyOpenMs(last),
			"source": "ws", "ready": true, "forming_bar_included": true,
			"timing": map[string]any{"rule": "hyperliquid_native_close", "interval_ms": restingTPPyHourMs, "bars": timing},
		}},
		"mids": map[string]any{"BTC": map[string]any{"px": frame[last][3], "recv_at_ms": cutoff, "source": "ws",
			"age_ms": 0, "stale": false, "confirmed": true}},
	}
}

func restingTPPyCheck(t *testing.T, python, root string, pos *Position, ctx PositionCtx, bars [][6]float64, tiers []map[string]float64, closedThrough int) StrategyDecisionFields {
	t.Helper()
	refs, err := json.Marshal(map[string]any{"open": map[string]any{"name": "breakout", "params": map[string]any{}},
		"closes": []any{map[string]any{"name": "tiered_tp_atr", "params": map[string]any{"tp_tiers": tiers}}}})
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{filepath.Join(root, "shared_scripts", "check_hyperliquid.py"), "breakout", "BTC", "1h", "--mode=paper",
		"--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", string(refs), "--position-side", pos.Side,
		"--position-avg-cost=100", "--position-qty=" + formatFloat(pos.Quantity), "--position-initial-qty=1",
		"--position-entry-atr=2"}
	argv, err = appendRestingTPRuleArg(argv, ctx)
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := json.Marshal(map[string]any{"v": 2, "market": restingTPPyMarket(bars, closedThrough)})
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
	return res.StrategyDecisionFields
}

func formatFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func restingTPPyBarIndex(ms int64) int {
	if ms <= 0 {
		return -1
	}
	return int((ms - restingTPPyT0) / restingTPPyHourMs)
}

func TestRestingTPRuleGoAndCheckScanAcrossChecks(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	useHLLotMetadataForTest(t, map[string]int{"BTC": 2})
	stopTiers := []map[string]float64{{"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}}
	trailTiers := []map[string]float64{{"atr_multiple": 1.5, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}}
	stopBarTail := func(bar63Low float64) []restingTPPyBar {
		return []restingTPPyBar{{100.0, 102.3, 99.9, 102.2}, {102.2, 103.8, 103.2, 103.6}, {103.6, 105.8, 102.9, 105.5},
			{105.5, 106.01, bar63Low, 105.9}, {105.9, 106.0, 105.5, 105.8}}
	}
	scenarios := []restingTPPyScenario{
		{
			name: "long stop bar the paper stop did not book drops only that bar", side: "long", tail: stopBarTail(105.2), tiers: stopTiers,
			steps: []restingTPPyStep{
				{closedThrough: 60, stop: 98, wantSent: 98, wantFraction: 0.5, wantFillPx: 102.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 60, wantReach: true, wantStoredStop: 98},
				{closedThrough: 61, stop: 103, wantSent: 98, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 103},
				{closedThrough: 62, stop: 103, wantSent: 103, wantStopBar: 62, wantRecorded: true, wantWatermark: 62, wantReach: true, wantStoredStop: 103},
				{closedThrough: 63, stop: 103, wantSent: 103, wantFraction: 1.0, wantFillPx: 106.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 63, wantReach: true, wantStoredStop: 103},
			},
		},
		{
			name: "long later stop bar still ends the scan", side: "long", tail: stopBarTail(102.95), tiers: stopTiers,
			steps: []restingTPPyStep{
				{closedThrough: 60, stop: 98, wantSent: 98, wantFraction: 0.5, wantFillPx: 102.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 60, wantReach: true, wantStoredStop: 98},
				{closedThrough: 61, stop: 103, wantSent: 98, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 103},
				{closedThrough: 62, stop: 103, wantSent: 103, wantStopBar: 62, wantRecorded: true, wantWatermark: 62, wantReach: true, wantStoredStop: 103},
				{closedThrough: 63, stop: 103, wantSent: 103, wantStopBar: 63, wantRecorded: true, wantWatermark: 63, wantReach: true, wantStoredStop: 103},
			},
		},
		{
			name: "long trailing ratchet inside a bar records the phase 1 trigger", side: "long", tiers: trailTiers,
			tail: []restingTPPyBar{{100.0, 100.2, 99.8, 100.0}, {100.0, 103.5, 99.5, 103.0}, {103.0, 103.2, 102.8, 103.0}},
			steps: []restingTPPyStep{
				{closedThrough: 60, stop: 99.0, stopAfterCheck: 101.0, wantSent: 99.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 60, wantReach: true, wantStoredStop: 99.0},
				{closedThrough: 60, stop: 102.4, wantSent: 99.0, wantStopBar: -1, wantRecorded: false, wantWatermark: 60, wantReach: true, wantStoredStop: 99.0},
				{closedThrough: 61, stop: 102.4, stopAfterCheck: 103.5, wantSent: 99.0, wantFraction: 0.5, wantFillPx: 103.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 102.4},
			},
		},
		{
			name: "short sends the looser stored trigger", side: "short", tiers: trailTiers,
			tail: []restingTPPyBar{{100.0, 100.2, 99.8, 100.0}, {100.0, 104.0, 96.5, 97.0}, {97.0, 97.2, 96.8, 97.0}},
			steps: []restingTPPyStep{
				{closedThrough: 60, stop: 105.0, wantSent: 105.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 60, wantReach: true, wantStoredStop: 105.0},
				{closedThrough: 61, stop: 103.0, wantSent: 105.0, wantFraction: 0.5, wantFillPx: 97.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 103.0},
			},
		},
		{
			name: "a zero trigger on either side sends no stop and an unarmed current stop holds", side: "long", tiers: trailTiers,
			tail: []restingTPPyBar{{100.0, 100.2, 99.8, 100.0}, {100.0, 103.5, 98.0, 103.0}, {103.0, 103.2, 102.8, 103.0}},
			preset: func(pos *Position) {
				pos.RestingTPScannedOpenMs = restingTPPyOpenMs(60)
				pos.RestingTPReachPx = 100.0
				pos.RestingTPStopTriggerPx = 0
			},
			steps: []restingTPPyStep{
				{closedThrough: 61, stop: 0, wantSent: 0, wantHeld: true, wantStopBar: -1, wantRecorded: false, wantWatermark: 60, wantReach: true, wantStoredStop: 0},
				{closedThrough: 61, stop: 99.0, wantSent: 0, wantFraction: 0.5, wantFillPx: 103.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 99.0},
			},
		},
		{
			name: "a stop bar on the first scan moves the watermark with no reach", side: "long", tiers: trailTiers,
			tail: []restingTPPyBar{{100.0, 100.2, 99.8, 100.0}, {101.5, 103.5, 101.2, 103.2}, {103.2, 103.4, 103.0, 103.2}},
			steps: []restingTPPyStep{
				{closedThrough: 60, stop: 101.0, wantSent: 101.0, wantStopBar: 60, wantRecorded: true, wantWatermark: 60, wantReach: false, wantStoredStop: 101.0},
				{closedThrough: 61, stop: 101.0, wantSent: 101.0, wantFraction: 0.5, wantFillPx: 103.0, wantStopBar: -1, wantRecorded: true, wantWatermark: 61, wantReach: true, wantStoredStop: 101.0},
			},
		},
	}
	for _, sn := range scenarios {
		t.Run(sn.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "resting-py", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
				Args: []string{"breakout", "BTC", "1h", "--mode=paper"}, RestingTPTradeThrough: true, StopLossPct: restingTPPyFloat(5)}
			pos := &Position{Symbol: "BTC", Side: sn.side, Quantity: 1, InitialQuantity: 1, AvgCost: 100, EntryATR: 2,
				OpenedAt: time.UnixMilli(restingTPPyOpenMs(60) + 600_000)}
			if sn.preset != nil {
				sn.preset(pos)
			}
			state := &StrategyState{ID: sc.ID, Cash: 1000, Positions: map[string]*Position{"BTC": pos}}
			bars := restingTPPyBars(sn.tail)
			var mu sync.RWMutex
			for i, step := range sn.steps {
				pos.StopLossTriggerPx = step.stop
				ctx := positionCtxForCheck(sc, pos, nil)
				if ctx.RestingTP == nil {
					t.Fatalf("step %d: no rule request", i+1)
				}
				if ctx.RestingTP.StopTriggerPx != step.wantSent {
					t.Fatalf("step %d: sent stop_trigger_px %v, want %v", i+1, ctx.RestingTP.StopTriggerPx, step.wantSent)
				}
				fields := restingTPPyCheck(t, python, root, pos, ctx, bars, sn.tiers, step.closedThrough)
				if msg := restingTPRuleContractError(ctx.RestingTP, sn.side, fields); msg != "" {
					t.Fatalf("step %d: contract error: %s", i+1, msg)
				}
				echo := fields.RestingTPRule
				if echo.Held != step.wantHeld {
					t.Fatalf("step %d: held %v (%s), want %v", i+1, echo.Held, echo.HoldReason, step.wantHeld)
				}
				if math.Abs(fields.CloseFraction-step.wantFraction) > 1e-9 || math.Abs(fields.CloseTierFillPrice-step.wantFillPx) > 1e-9 {
					t.Fatalf("step %d: close_fraction %v at %v, want %v at %v", i+1, fields.CloseFraction, fields.CloseTierFillPrice, step.wantFraction, step.wantFillPx)
				}
				if got := restingTPPyBarIndex(echo.StopReachedBarOpenMs); got != step.wantStopBar {
					t.Fatalf("step %d: stop-reached bar %d, want %d", i+1, got, step.wantStopBar)
				}
				if step.stopAfterCheck > 0 {
					pos.StopLossTriggerPx = step.stopAfterCheck
				}
				mu.Lock()
				recorded := recordRestingTPScan(state, "BTC", ctx.RestingTP, fields)
				mu.Unlock()
				if recorded != step.wantRecorded {
					t.Fatalf("step %d: recorded %v, want %v", i+1, recorded, step.wantRecorded)
				}
				if got := restingTPPyBarIndex(pos.RestingTPScannedOpenMs); got != step.wantWatermark {
					t.Fatalf("step %d: stored watermark bar %d, want %d", i+1, got, step.wantWatermark)
				}
				if (pos.RestingTPReachPx > 0) != step.wantReach {
					t.Fatalf("step %d: stored reach %v, want set=%v", i+1, pos.RestingTPReachPx, step.wantReach)
				}
				if pos.RestingTPStopTriggerPx != step.wantStoredStop {
					t.Fatalf("step %d: stored stop %v, want %v", i+1, pos.RestingTPStopTriggerPx, step.wantStoredStop)
				}
				if fields.CloseFraction >= 1 {
					pos.Quantity = 0
				} else if fields.CloseFraction > 0 {
					pos.Quantity *= 1 - fields.CloseFraction
				}
			}
		})
	}
}

func restingTPPyFloat(v float64) *float64 { return &v }
