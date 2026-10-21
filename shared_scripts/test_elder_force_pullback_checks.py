import json
import os
import subprocess
import sys

import numpy as np
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "elder_force_pullback"
H = 3_600_000
T0 = 1_772_323_200_000
CROSS_AT = 211
TIERS = [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
         {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": TIERS}}


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run([sys.executable, CHECK_HL] + argv, input="" if stdin is None else json.dumps(stdin),
                          capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _closes(sign=1, n=260):
    step = np.full(n, 0.5)
    step[200:205] = -2.0
    step[0] = 0.0
    return 100.0 + np.cumsum(step) if sign > 0 else 400.0 - np.cumsum(step)


def _bars(last_closed, sign=1):
    close = _closes(sign)
    end = last_closed + 2
    out = []
    for i in range(end - 200, end):
        t = T0 + i * H
        o = float(close[i - 1])
        c = float(close[i])
        out.append({"t": t, "T": t + H - 1, "o": o, "h": max(o, c) + 0.5, "l": min(o, c) - 0.5, "c": c, "v": 100.0})
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


def _refs(params=None):
    return json.dumps({"open": {"name": NAME, "params": dict(params or {})}, "closes": [TP]})


def _check(bars, *, extra=(), mid=None, params=None, closed=True):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
            "--strategy-refs", _refs(params), *(["--closed-bar-decisions"] if closed else []), *extra]
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


@pytest.mark.parametrize("sign,signal,reason", [(1, 1, "entry_long"), (-1, -1, "entry_short")])
def test_closed_bar_paper_check_decides_on_the_closed_crossing_bar(sign, signal, reason):
    bars = _bars(CROSS_AT, sign)
    out = _check(bars)
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1
    assert out["signal"] == signal and out["indicator_labels"]["efp_reason"] == reason
    ind = out["indicators"]
    assert ind["efp_valid"] == 1.0 and ind["efp_cross"] == float(signal)
    assert ind["efp_eval_ts_ms"] == float(bars[-2]["t"]) and ind["efp_required_history"] == 152.0
    assert ind["efp_raw_force"] == (bars[-2]["c"] - bars[-3]["c"]) * 100.0
    assert np.sign(ind["efp_prev_force"]) == -signal and np.sign(ind["efp_force"]) == signal


def test_a_forming_crossing_candle_or_its_volume_cannot_authorize_an_entry():
    bars = _bars(CROSS_AT - 1)
    out = _check(bars)
    assert out["signal"] == 0 and out["indicator_labels"]["efp_reason"] == "no_cross"
    assert out["indicators"]["efp_eval_ts_ms"] == float(bars[-2]["t"])
    extreme = [dict(b) for b in bars]
    extreme[-1].update({"v": 1e9, "c": bars[-1]["c"] + 50.0, "h": bars[-1]["c"] + 51.0})
    got = _check(extreme)
    assert got["signal"] == 0 and got["indicators"] == out["indicators"]
    closed = _check(_bars(CROSS_AT))
    assert closed["signal"] == 1


def test_legacy_checks_without_closed_bar_decisions_decide_on_the_forming_row():
    bars = _bars(CROSS_AT - 1)
    legacy = _check(bars, closed=False)
    assert "closed_bar_decision" not in legacy or legacy["closed_bar_decision"] is None
    assert legacy["signal"] == 1 and legacy["indicator_labels"]["efp_reason"] == "entry_long"
    assert legacy["indicators"]["efp_close"] == bars[-1]["c"] and legacy["indicators"]["efp_volume"] == bars[-1]["v"]


@pytest.mark.parametrize("defect", ["gap", "geometry"])
def test_invalid_entry_data_holds_the_entry_and_keeps_close_management(defect):
    bars = _bars(CROSS_AT)
    if defect == "gap":
        del bars[150]
    else:
        bars[150]["h"] = bars[150]["l"] - 1.0
    flat = _check(bars)
    assert flat["signal"] == 0 and flat["open_action"] == "none"
    if defect == "gap":
        assert flat["closed_bar_decision"]["held"] is False
        assert flat["indicator_labels"]["efp_reason"] == "cadence_gap" and flat["indicators"]["efp_valid"] == 0.0
    else:
        assert flat["closed_bar_decision"]["held"] is True and "high below low" in flat["closed_bar_decision"][
            "hold_reason"]
    held = ["--position-side", "long", "--position-avg-cost=90", "--position-qty=1", "--position-initial-qty=1"]
    out = _check(bars, extra=held, mid=float(bars[-2]["c"]))
    assert out["open_action"] == "none" and out["close_strategy"] == "tiered_tp_pct"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1


@pytest.mark.parametrize("params,needle", [
    ({"recursion_length": 40}, "recursion_length"),
    ({"volume_weighting": True}, "volume_weighting"),
    ({"trend_period": 190, "slope_span": 9}, "required history"),
])
def test_invalid_parameters_are_refused_before_any_candle_fetch(params, needle):
    proc = _run([NAME, "BTC", "1h", "--mode=paper", "--strategy-refs", _refs(params)])
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and needle in out["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in out["error"]
    assert "Fetching BTC" not in proc.stderr


def test_a_mixed_batch_refuses_the_invalid_slot_and_matches_the_solo_check():
    bars = _bars(CROSS_AT)
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(_refs())}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"recursion_length": 40}}
    fallback = {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
                "open_strategy": "breakout", "close_strategies": [NAME],
                "close_params_by_name": {NAME: {"force_period": 1}}}
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                {"v": 2, "slots": [good, bad, fallback], "market": _market(bars)})
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "recursion_length" in results["bad"]["error"]
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"]
    solo = _check(bars)
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), key
    assert solo["signal"] == 1
    assert results["good"]["indicators"]["efp_force"] == solo["indicators"]["efp_force"]
