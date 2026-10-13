//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func elderForceMarket(lastClosed int) map[string]any {
	const hour = int64(3_600_000)
	t0 := int64(1_772_323_200_000)
	end := lastClosed + 2
	closeAt := func(i int) float64 {
		c := 100.0
		for k := 1; k <= i; k++ {
			if k >= 200 && k < 205 {
				c -= 2.0
			} else {
				c += 0.5
			}
		}
		return c
	}
	rows := make([][]float64, 0, 200)
	timing := make([][]any, 0, 200)
	var last float64
	for i := end - 200; i < end; i++ {
		o, c := closeAt(i-1), closeAt(i)
		hi, lo := o, c
		if c > o {
			hi, lo = c, o
		}
		open := t0 + int64(i)*hour
		closeMs := open + hour - 1
		rows = append(rows, []float64{float64(closeMs), o, hi + 0.5, lo - 0.5, c, 100})
		timing = append(timing, []any{open, closeMs, true})
		last = c
	}
	firstOpen := t0 + int64(end-200)*hour
	lastOpen := t0 + int64(end-1)*hour
	cutoff := lastOpen + hour/2
	return map[string]any{
		"version": 1, "snapshot_id": "efp/1", "generation": 1, "sealed_at_ms": cutoff, "decision_cutoff_ms": cutoff,
		"feed_complete": true,
		"frames": map[string]any{"BTC|1h": map[string]any{
			"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": false,
			"first_open_ms": firstOpen, "last_open_ms": lastOpen, "last_close_ms": lastOpen + hour - 1,
			"last_recv_at_ms": cutoff, "source": "ws", "ready": true, "forming_bar_included": true,
			"timing": map[string]any{"rule": "hyperliquid_native_close", "interval_ms": hour, "bars": timing},
		}},
		"mids": map[string]any{"BTC": map[string]any{"px": last, "recv_at_ms": cutoff, "source": "ws",
			"age_ms": 0, "stale": false, "confirmed": true}},
	}
}

func TestElderForcePullbackAddedPaperEntryRunsThroughTheRealCheckScript(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	setNoEdgeLiveCredentials(t)
	_, entry, err := buildAddStrategyEntry(elderForceName, "hyperliquid", "btc")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(entry, &raw); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(writeNoEdgeConfig(t, CurrentConfigVersion, raw))
	if err != nil {
		t.Fatalf("added paper entry must load: %v", err)
	}
	sc := cfg.Strategies[0]
	refs, err := buildStrategyRefsArg(sc, "", false)
	if err != nil || len(refs) != 2 {
		t.Fatalf("strategy refs = %v, %v", refs, err)
	}
	for _, tc := range []struct {
		name       string
		lastClosed int
		signal     int
		reason     string
	}{
		{"closed crossing bar opens a long", 211, 1, "entry_long"},
		{"forming crossing bar cannot open", 210, 0, "no_cross"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := append([]string{filepath.Join(root, sc.Script)}, sc.Args...)
			argv = append(argv, "--market-stdin", "--ohlcv-limit", "200", closedBarDecisionsFlag)
			argv = append(argv, refs...)
			stdin, err := json.Marshal(map[string]any{"v": 2, "market": elderForceMarket(tc.lastClosed)})
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
			if msg := closedBarDecisionContractError(sc, res.StrategyDecisionFields); msg != "" {
				t.Fatalf("closed-bar contract: %s", msg)
			}
			var labels struct {
				IndicatorLabels map[string]string `json:"indicator_labels"`
			}
			_ = json.Unmarshal(out, &labels)
			if res.Signal != tc.signal || labels.IndicatorLabels["efp_reason"] != tc.reason {
				t.Fatalf("signal %d reason %q, want %d %q\n%s", res.Signal, labels.IndicatorLabels["efp_reason"], tc.signal, tc.reason, out)
			}
		})
	}
}
