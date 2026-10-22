import json
import os
import subprocess
import sys

import numpy as np
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "mass_index_reversal"
H = 3_600_000
T0 = 1_772_323_200_000
UP_AT, DOWN_AT = 205, 217
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


def _series(slope=0.05, n=260):
    rng = np.ones(n)
    rng[200:208] = 3.0
    mid = 100.0 + slope * np.arange(n)
    mid[DOWN_AT:] = mid[DOWN_AT:] - 5.0 * np.sign(slope)
    return rng, mid


def _bars(last_closed, slope=0.05, forming=True):
    rng, mid = _series(slope)
    end = last_closed + (2 if forming else 1)
    out = []
    for i in range(end - 200, end):
        t = T0 + i * H
        out.append({"t": t, "T": t + H - 1, "o": float(mid[i]), "h": float(mid[i] + rng[i] / 2),
                    "l": float(mid[i] - rng[i] / 2), "c": float(mid[i]), "v": 100.0})
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
    return {"version": 4, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": cutoff,
            "decision_cutoff_ms": cutoff, "frames": {"BTC|1h": _frame(bars)}, "feed_complete": True,
            "mids": {"BTC": {"px": bars[-1]["c"] if mid is None else mid, "recv_at_ms": cutoff, "source": "ws",
                             "age_ms": 0, "stale": False, "confirmed": True}}}


def _refs(params=None):
    return json.dumps({"open": {"name": NAME, "params": dict(params or {})}, "closes": [TP]})


def _check(bars, *, extra=(), mid=None, params=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
            "--strategy-refs", _refs(params), "--closed-bar-decisions", *extra]
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
    error = _payload(proc)["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in error and "missing mode" in error


def _assert_closed_completion(out, bars, signal, reason, case):
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == signal and out["indicator_labels"]["mir_reason"] == reason, case
    ind = out["indicators"]
    assert ind["mir_valid"] == 1.0 and ind["mir_bulge_completed"] == 1.0, case
    assert ind["mir_eval_ts_ms"] == float(bars[-2]["t"]), case
    assert ind["mir_bulge_armed_ts_ms"] == float(T0 + UP_AT * H), case
    assert ind["mir_bulge_age"] == float(DOWN_AT - UP_AT) and ind["mir_required_history"] == 148.0, case
    assert ind["mir_mass_index"] < 26.5 <= ind["mir_prev_mass_index"], case


def test_closed_short_completion_bar_authorizes_the_entry_a_forming_one_cannot_and_the_mixed_batch_matches():
    bars = _bars(DOWN_AT)
    solo = _check(bars)
    _assert_closed_completion(solo, bars, -1, "entry_short",
                              "closed_bar_paper_check_decides_on_the_closed_completion_bar[entry_short]")
    case = "a_forming_completion_candle_cannot_authorize_an_entry"
    forming = _bars(DOWN_AT - 1)
    out = _check(forming)
    assert out["signal"] == 0 and out["indicator_labels"]["mir_reason"] == "armed", case
    assert out["indicators"]["mir_eval_ts_ms"] == float(forming[-2]["t"]), case
    extreme = [dict(b) for b in forming]
    extreme[-1].update({"c": 1.0, "l": 0.5, "o": 1.0})
    out = _check(extreme)
    assert out["signal"] == 0 and out["indicators"]["mir_eval_ts_ms"] == float(forming[-2]["t"]), case
    assert solo["signal"] == -1, case
    case = "a_mixed_batch_refuses_the_invalid_slot_and_matches_the_solo_check"
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(_refs())}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"recursion_length": 40}}
    fallback = {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
                "open_strategy": "breakout", "close_strategies": [NAME],
                "close_params_by_name": {NAME: {"ema_period": 1}}}
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                {"v": 2, "slots": [good, bad, fallback], "market": _market(bars)})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "recursion_length" in results["bad"]["error"], case
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == -1, case


def test_closed_bar_paper_check_decides_on_the_closed_long_completion_bar():
    bars = _bars(DOWN_AT, -0.05)
    _assert_closed_completion(_check(bars), bars, 1, "entry_long",
                              "closed_bar_paper_check_decides_on_the_closed_completion_bar[entry_long]")


@pytest.mark.parametrize("defect", ["gap", "geometry"])
def test_invalid_entry_data_holds_the_entry_and_keeps_close_management(defect):
    bars = _bars(DOWN_AT)
    if defect == "gap":
        del bars[150]
    else:
        bars[150]["h"] = bars[150]["l"] - 1.0
    flat = _check(bars)
    assert flat["signal"] == 0 and flat["open_action"] == "none"
    if defect == "gap":
        assert flat["closed_bar_decision"]["held"] is False
        assert flat["indicator_labels"]["mir_reason"] == "cadence_gap" and flat["indicators"]["mir_valid"] == 0.0
    else:
        assert flat["closed_bar_decision"]["held"] is True and "high below low" in flat["closed_bar_decision"][
            "hold_reason"]
    held = ["--position-side", "long", "--position-avg-cost=90", "--position-qty=1", "--position-initial-qty=1"]
    out = _check(bars, extra=held, mid=float(bars[-2]["c"]))
    assert out["open_action"] == "none" and out["close_strategy"] == "tiered_tp_pct"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1


@pytest.mark.parametrize("params,needle", [
    ({"recursion_length": 40}, "recursion_length"),
    ({"bulge_filter": True}, "bulge_filter"),
    ({"max_bulge_age": 150}, "required history"),
])
def test_invalid_parameters_are_refused_before_any_candle_fetch(params, needle):
    proc = _run([NAME, "BTC", "1h", "--mode=paper", "--strategy-refs", _refs(params)])
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and needle in out["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in out["error"]
    assert "Fetching BTC" not in proc.stderr
