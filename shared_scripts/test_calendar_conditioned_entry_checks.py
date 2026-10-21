import json
import os
import random
import subprocess
import sys

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "calendar_conditioned_entry"
H = 3_600_000
FORMING = 1_772_460_000_000
SMALL = {"training_bars": 672, "holding_bars": 1, "refit_bars": 24, "min_effective_obs": 2, "confidence": 0.5,
         "cost_hurdle": 0.0, "expected_interval_ms": H}
TIME_STOP = {"name": "time_stop", "params": {"max_bars": 1}}
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run([sys.executable, CHECK_HL] + argv, input="" if stdin is None else json.dumps(stdin),
                          capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _bars(n=800, effect=0.02):
    rng = random.Random(1653)
    out = []
    price = 100.0
    for k in range(n):
        t = FORMING - (n - 1 - k) * H
        rel = t - 345_600_000
        cell = ((rel // 86_400_000) % 7) * 24 + (rel % 86_400_000) // H
        move = rng.gauss(0.0, 0.001) + (effect if cell == 14 else 0.0)
        close = price * (1.0 + move)
        out.append({"t": t, "T": t + H - 1, "o": price, "h": max(price, close) * 1.0002,
                    "l": min(price, close) * 0.9998, "c": close, "v": 1000.0})
        price = close
    return out


def _frame(bars):
    rows = [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]
    return {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
            "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
            "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True,
            "timing": {"rule": "hyperliquid_native_close", "interval_ms": H,
                       "bars": [[b["t"], b["T"], True] for b in bars]}}


def _market(bars, mid=None):
    cutoff = bars[-1]["t"] + H // 2
    return {"version": 3, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": cutoff,
            "decision_cutoff_ms": cutoff, "frames": {"BTC|1h": _frame(bars)}, "feed_complete": True,
            "mids": {"BTC": {"px": bars[-1]["c"] if mid is None else mid, "recv_at_ms": cutoff, "source": "ws",
                             "age_ms": 0, "stale": False, "confirmed": True}}}


def _refs(params, closes=(TIME_STOP,)):
    return json.dumps({"open": {"name": NAME, "params": dict(params)}, "closes": list(closes)})


def _check(bars, *, params=None, closed=True, extra=(), closes=(TIME_STOP,), limit=800, mid=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", str(limit),
            "--strategy-refs", _refs(SMALL if params is None else params, closes), *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    proc = _run(argv, {"v": 2, "market": _market(bars, mid)})
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-2000:]
    return _payload(proc)


def test_missing_mode_is_refused_for_the_open_reference():
    proc = _run([NAME, "BTC", "1h"])
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0 and payload["strategy"] == NAME
    assert f"open strategy '{NAME}' is edge_status=no_edge" in payload["error"]
    assert "missing mode" in payload["error"]


def test_missing_mode_is_refused_for_a_close_fallback_reference():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})
    proc = _run(["breakout", "BTC", "1h", "--strategy-refs", refs])
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert f"close strategy '{NAME}' is edge_status=no_edge" in payload["error"] and "missing mode" in payload["error"]


def test_explicit_paper_check_decides_on_the_closed_bar_for_the_next_calendar_cell():
    bars = _bars()
    out = _check(bars)
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1
    assert out["signal"] == 1 and out["open_action"] == "long"
    assert out["indicator_labels"]["cce_reason"] == "entry_long"
    ind = out["indicators"]
    assert ind["cce_valid"] == 1.0 and ind["cce_eval_ts_ms"] == float(bars[-2]["t"])
    assert ind["cce_next_open_ms"] == float(bars[-1]["t"]) and ind["cce_next_cell"] == 14.0
    assert ind["cce_decision_boundary_ms"] == float(bars[-2]["t"] + H)
    assert ind["cce_decision_cutoff_ms"] == float(bars[-1]["t"] + H // 2)
    assert ind["cce_family_size"] == 336.0 and ind["cce_long_lower_bound"] > 0.0
    assert ind["cce_long_net_edge"] == ind["cce_gross_mean"] - SMALL["cost_hurdle"]
    assert ind["cce_refit_boundary_ms"] <= ind["cce_decision_boundary_ms"]
    assert ind["cce_gross_mean"] != round(ind["cce_gross_mean"], 6)


def test_the_production_frame_and_default_parameters_hold_for_missing_training_history():
    bars = _bars(n=201)
    out = _check(bars, params={}, limit=200, closes=({"name": "time_stop", "params": {"max_bars": 4}},))
    assert out["closed_bar_decision"]["held"] is False
    assert out["signal"] == 0 and out["open_action"] == "none"
    assert out["indicator_labels"]["cce_reason"] == "insufficient_history"
    assert out["indicators"]["cce_required_bars"] >= 4368.0 and out["indicators"]["cce_history_bars"] == 200.0


def test_without_closed_bar_decisions_the_check_frame_holds_every_entry():
    bars = _bars()
    out = _check(bars, closed=False)
    assert "closed_bar_decision" not in out
    assert out["signal"] == 0 and out["open_action"] == "none"
    assert out["indicator_labels"]["cce_reason"] == "undeclared_timing"
    assert _check(bars)["signal"] == 1


def test_an_entry_data_hold_keeps_close_management_for_an_open_position():
    bars = _bars()
    bars[-60]["c"] = bars[-60]["h"] * 1.01
    flat = _check(bars)
    assert flat["signal"] == 0 and flat["indicator_labels"]["cce_reason"] == "history_inconsistent_candle"
    held = ["--position-side", "long", "--position-avg-cost=90", "--position-qty=1", "--position-initial-qty=1"]
    out = _check(bars, extra=held, closes=(TP,), mid=bars[-2]["c"])
    assert out["open_action"] == "none" and out["close_strategy"] == "tiered_tp_pct"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    live = _check(bars, extra=held)
    assert live["close_strategy"] in ("", "time_stop") and live["close_fraction"] == 0.0 and live["signal"] == 0


def test_invalid_parameters_are_refused_before_any_candle_fetch():
    refs = _refs({**SMALL, "refit_bars": 700})
    proc = _run([NAME, "BTC", "1h", "--mode=paper", "--strategy-refs", refs])
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and f"open strategy '{NAME}' has invalid parameters" in out["error"]
    assert "refit_bars" in out["error"] and "Fetching BTC" not in proc.stderr


def test_a_cadence_mismatch_is_refused_before_any_candle_fetch():
    refs = json.dumps({"open": {"name": NAME}, "closes": [TIME_STOP]})
    proc = _run([NAME, "BTC", "4h", "--mode=paper", "--strategy-refs", refs])
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and "registry default expected_interval_ms=3600000" in out["error"]
    assert "Fetching BTC" not in proc.stderr


def test_a_mixed_batch_evaluates_valid_peers_and_matches_the_solo_check():
    bars = _bars()
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(_refs(SMALL))}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {**SMALL, "confidence": 1.5}}
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "800", "--market-stdin"],
                {"v": 2, "slots": [good, bad], "market": _market(bars)})
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "has invalid parameters" in results["bad"]["error"]
    solo = _check(bars)
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), key
    assert solo["signal"] == 1


@pytest.mark.parametrize("effect", [0.0])
def test_a_frame_without_a_calendar_effect_holds_below_the_corrected_threshold(effect):
    out = _check(_bars(effect=effect))
    assert out["signal"] == 0 and out["indicator_labels"]["cce_reason"] in ("below_threshold",
                                                                           "insufficient_effective_obs")
    assert out["indicators"]["cce_valid"] == 1.0
