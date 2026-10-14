//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const bllPythonDriver = `
import json, sys
root, path = sys.argv[1], sys.argv[2]
sys.path[:0] = [root + "/shared_tools", root + "/backtest", root + "/shared_strategies/open"]
from lead_lag_inputs import prepare_sealed_inputs
from bitcoin_lead_lag import merged_params
from closed_bar import FRAME_DECISION_TIME_ATTR, declare_frame_cadence, timings_timestamp_kind
from registry_loader import load_registry
from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision
import pandas as pd

env = json.load(open(path))
market = env["market"]
params = {"training_bars": 240, "lag_bars": 2, "horizon_bars": 4, "max_data_age_ms": 86400000}
need = merged_params(params)["support_bars"]
out = {}
for label, mutate in (("sealed", None), ("leader_timing_dropped", "timing"), ("leader_duplicate", "dup"),
                      ("leader_frame_missing", "missing")):
    m = json.loads(json.dumps(market))
    btc = m["frames"].get("BTC|1h")
    if mutate == "timing":
        btc.pop("timing")
    elif mutate == "dup":
        btc["rows"][5] = list(btc["rows"][4])
        btc["timing"]["bars"][5] = list(btc["timing"]["bars"][4])
    elif mutate == "missing":
        m["frames"].pop("BTC|1h")
    rows, timings, ctx = prepare_sealed_inputs(m, "ETH", "1h", need)
    rec = {"available": ctx["available"], "hold_reason": ctx.get("hold_reason", "")}
    if rows is not None:
        df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
        df.index = pd.to_datetime(df["timestamp"], unit="ms")
        declare_frame_cadence(df, "1h", timings_timestamp_kind(rows, timings))
        df.attrs[FRAME_DECISION_TIME_ATTR] = {"cutoff_ms": ctx["cutoff_ms"], "boundary_ms": ctx["boundary_ms"]}
        rec.update(rows=len(rows), first_open=ctx["leader_open_ms"][0], last_open=ctx["leader_open_ms"][-1],
                   boundary=ctx["boundary_ms"], cutoff=ctx["cutoff_ms"],
                   follower_close=[r[4] for r in rows], leader_close=ctx["leader_close"])
    else:
        df = None
    reg = load_registry("futures")
    frame = df if df is not None else pd.DataFrame(
        [r for r in market["frames"]["ETH|1h"]["rows"]], columns=["timestamp", "open", "high", "low", "close", "volume"])
    if df is None:
        frame.index = pd.to_datetime(frame["timestamp"], unit="ms")
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, frame, "bitcoin_lead_lag", "bitcoin_lead_lag", ["tiered_tp_pct"], "long",
        params={**params, "leader_context": ctx},
        position_ctx={"side": "long", "avg_cost": 1.0, "entry_atr": 1.0, "current_quantity": 1.0, "initial_quantity": 1.0},
        close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}})
    decision = finalize_decision(evaluation, "long")
    last = evaluation.open_result_df.iloc[-1]
    rec.update(reason=str(last["bll_reason"]), open_action=decision["open_action"],
               close_fraction=decision["close_fraction"], close_strategy=decision.get("close_strategy"))
    out[label] = rec
print(json.dumps(out))
`

func TestBitcoinLeadLagGoPayloadParsesThroughThePythonInputsEvaluatorAndComposer(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	bll, momEth, _, _ := bllStrategies()
	req, cfg := bllDerive(t, bll, momEth)
	now := time.Unix(1_790_000_000, 0).UTC().Add(90 * time.Second)
	owner := bllOwner(t, req, now)
	snap := sealCycleMarketSnapshot(context.Background(), owner, cycleRequirementsForDue(cfg.Strategies, req), "300s/bllpy", now)
	feed := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: 300}
	blob, err := feed.singleCheckPayload(bll)
	if err != nil {
		t.Fatal(err)
	}
	p := bllDecode(t, blob)
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", "-c", bllPythonDriver, root, path)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("python driver: %v\n%s", err, stderr)
	}
	var got map[string]struct {
		Available     bool      `json:"available"`
		HoldReason    string    `json:"hold_reason"`
		Rows          int       `json:"rows"`
		FirstOpen     int64     `json:"first_open"`
		LastOpen      int64     `json:"last_open"`
		Boundary      int64     `json:"boundary"`
		Cutoff        int64     `json:"cutoff"`
		FollowerClose []float64 `json:"follower_close"`
		LeaderClose   []float64 `json:"leader_close"`
		Reason        string    `json:"reason"`
		OpenAction    string    `json:"open_action"`
		CloseFraction float64   `json:"close_fraction"`
		CloseStrategy string    `json:"close_strategy"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("driver output %s: %v", out, err)
	}
	sealed := got["sealed"]
	support := bllRows - 1
	eth, btc := p.Frames["ETH|1h"], p.Frames["BTC|1h"]
	lastClosedOpen := eth.LastOpenMs - bllHourMs
	if !sealed.Available || sealed.Rows != support || sealed.LastOpen != lastClosedOpen || sealed.Boundary != lastClosedOpen+bllHourMs || sealed.Cutoff != p.DecisionCutoffMs {
		t.Fatalf("python selection of the Go payload: %+v (want %d closed rows ending at %d, cutoff %d)", sealed, support, lastClosedOpen, p.DecisionCutoffMs)
	}
	for k := 0; k < support; k++ {
		if sealed.FollowerClose[k] != eth.Rows[k].Close || sealed.LeaderClose[k] != btc.Rows[k].Close {
			t.Fatalf("selected row %d differs from the Go frame rows", k)
		}
	}
	if sealed.OpenAction != "none" || sealed.CloseStrategy != "tiered_tp_pct" {
		t.Fatalf("composer decision on the sealed payload: %+v", sealed)
	}
	for label, want := range map[string]string{
		"leader_timing_dropped": "timing_invalid",
		"leader_duplicate":      "duplicate_bar",
		"leader_frame_missing":  "frame_missing",
	} {
		rec := got[label]
		if rec.Available || rec.HoldReason != want || rec.Reason != "leader_unavailable" || rec.OpenAction != "none" {
			t.Fatalf("%s: %+v, want an entry hold %q", label, rec, want)
		}
		if rec.CloseStrategy != "tiered_tp_pct" {
			t.Fatalf("%s: the configured close owner must still evaluate: %+v", label, rec)
		}
	}
}
