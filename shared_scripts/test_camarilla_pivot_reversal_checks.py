import json
import os
import subprocess
import sys

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "camarilla_pivot_reversal"
H = 3_600_000
DAY = 86_400_000
T0 = 1_772_323_200_000
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


def _bar(t, o, c, h=None, lo=None):
    return {"t": t, "T": t + H - 1, "o": o, "h": max(o, c) + 0.1 if h is None else h,
            "l": min(o, c) - 0.1 if lo is None else lo, "c": c, "v": 1000.0}


def _bars(current, days_before=8):
    out = []
    t = T0
    for _ in range(days_before):
        for k in range(24):
            if k == 0:
                out.append(_bar(t, 100.0, 100.0, h=110.0, lo=99.9))
            elif k == 23:
                out.append(_bar(t, 100.0, 100.0, h=100.1, lo=90.0))
            else:
                out.append(_bar(t, 100.0, 100.0))
            t += H
    prev = 100.0
    for c in current:
        out.append(_bar(t, prev, c))
        prev = c
        t += H
    return out


def _frame(bars):
    rows = [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]
    return {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
            "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
            "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True,
            "timing": {"rule": "hyperliquid_native_close", "interval_ms": H,
                       "bars": [[b["t"], b["T"], True] for b in bars]}}


def _market(bars, cutoff=None, mid=None):
    cutoff = bars[-1]["t"] + H // 2 if cutoff is None else cutoff
    return {"version": 4, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": cutoff,
            "decision_cutoff_ms": cutoff, "frames": {"BTC|1h": _frame(bars)}, "feed_complete": True,
            "mids": {"BTC": {"px": bars[-1]["c"] if mid is None else mid, "recv_at_ms": cutoff, "source": "ws",
                             "age_ms": 0, "stale": False, "confirmed": True}}}


def _refs(params=None, closes=(TP,)):
    return json.dumps({"open": {"name": NAME, "params": dict(params or {"expected_interval_ms": H})},
                       "closes": list(closes)})


def _check(bars, *, closed=True, extra=(), cutoff=None, mid=None, params=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
            "--strategy-refs", _refs(params), *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    proc = _run(argv, {"v": 2, "market": _market(bars, cutoff, mid)})
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


def _assert_closed_crossing(out, bars, signal, reason, case):
    meta = out["closed_bar_decision"]
    assert meta["held"] is False and meta["forming_rows_dropped"] == 1, case
    assert out["signal"] == signal and out["indicator_labels"]["cpr_reason"] == reason, case
    ind = out["indicators"]
    assert ind["cpr_valid"] == 1.0 and ind["cpr_eval_ts_ms"] == float(bars[-2]["t"]), case
    assert ind["cpr_decision_boundary_ms"] == float(bars[-2]["t"] + H), case
    assert ind["cpr_decision_cutoff_ms"] == float(bars[-1]["t"] + H // 2), case
    assert (ind["cpr_prior_high"], ind["cpr_prior_low"], ind["cpr_prior_close"]) == (110.0, 90.0, 100.0), case
    rng = ind["cpr_prior_high"] - ind["cpr_prior_low"]
    assert ind["cpr_s3"] == ind["cpr_prior_close"] - 1.1 * rng / 4, case
    assert ind["cpr_r4"] == ind["cpr_prior_close"] + 1.1 * rng / 2, case
    assert (ind["cpr_prev_close"], ind["cpr_close"]) == (bars[-3]["c"], bars[-2]["c"]), case
    assert ind["cpr_prior_day_open_ms"] == ind["cpr_day_open_ms"] - DAY, case


def test_closed_bar_long_crossing_decides_on_the_closed_candle_and_anchors_the_frame_hold_and_mixed_batch():
    bars = _bars([100.0, 94.0, 95.0, 96.0])
    solo = _check(bars)
    _assert_closed_crossing(solo, bars, 1, "entry_long",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_candle[entry_long]")
    case = "without_closed_bar_decisions_the_check_frame_holds_every_entry"
    out = _check(bars, closed=False)
    assert "closed_bar_decision" not in out, case
    assert out["signal"] == 0 and out["open_action"] == "none", case
    assert out["indicator_labels"]["cpr_reason"] == "undeclared_timing", case
    assert out["indicators"]["cpr_valid"] == 0.0, case
    assert solo["signal"] == 1, case
    case = "a_mixed_batch_evaluates_valid_peers_and_matches_the_solo_check"
    market = _market(bars)
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
            "closed_bar_decisions": True, "strategy_refs": json.loads(_refs())}
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "closed_bar_decisions": True, "params": {"expected_interval_ms": 4 * H}}
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--market-stdin"],
                {"v": 2, "slots": [good, bad], "market": market})
    assert proc.returncode == 1, case
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "does not match timeframe '1h'" in results["bad"]["error"], case
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels", "closed_bar_decision"):
        assert results["good"].get(key) == solo.get(key), (case, key)
    assert solo["signal"] == 1, case


def test_closed_bar_paper_check_decides_on_the_closed_short_crossing_candle():
    bars = _bars([100.0, 106.0, 105.0, 104.0])
    _assert_closed_crossing(_check(bars), bars, -1, "entry_short",
                            "closed_bar_paper_check_decides_on_the_closed_crossing_candle[entry_short]")


def test_a_stale_closed_bar_holds_and_a_forming_crossing_waits_for_its_close():
    bars = _bars([100.0, 94.0, 95.0])
    stale = _check(bars, cutoff=bars[-1]["t"] + 2 * H + 1)
    assert stale["signal"] == 0 and stale["indicator_labels"]["cpr_reason"] == "decision_bar_stale"
    forming = _check(bars)
    assert forming["signal"] == 0 and forming["indicators"]["cpr_eval_ts_ms"] == float(bars[-2]["t"])
    closed = _check(bars + [_bar(bars[-1]["t"] + H, 95.0, 95.5)])
    assert closed["signal"] == 1 and closed["indicators"]["cpr_eval_ts_ms"] == float(bars[-1]["t"])


def test_invalid_entry_data_holds_the_entry_and_keeps_close_management():
    bars = _bars([100.0, 94.0, 95.0, 96.0])
    bars[187]["c"] = bars[187]["h"] + 5.0
    flat = _check(bars)
    assert flat["signal"] == 0 and flat["indicator_labels"]["cpr_reason"] == "inconsistent_candle"
    held = ["--position-side", "long", "--position-avg-cost=90", "--position-qty=1", "--position-initial-qty=1"]
    out = _check(bars, extra=held, mid=96.0)
    assert out["open_action"] == "none" and out["close_strategy"] == "tiered_tp_pct"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1


@pytest.mark.parametrize("timeframe,params,needle", [
    ("4h", {"expected_interval_ms": H}, "does not match timeframe '4h'"),
    ("4h", None, "registry default expected_interval_ms=3600000"),
    ("5m", {"expected_interval_ms": 300_000}, "expected_interval_ms >= 900000"),
    ("6h", {"expected_interval_ms": 21_600_000}, "not a supported interval"),
    ("1h", {"expected_interval_ms": True}, "expected_interval_ms"),
])
def test_cadence_errors_are_refused_before_any_candle_fetch(timeframe, params, needle):
    refs = json.dumps({"open": {"name": NAME, "params": params or {}}, "closes": [TP]})
    proc = _run([NAME, "BTC", timeframe, "--mode=paper", "--strategy-refs", refs])
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and needle in out["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in out["error"]
    assert "Fetching BTC" not in proc.stderr


def test_a_batch_with_only_invalid_slots_refuses_every_slot_without_building_shared_state():
    slots = [
        {"id": "cadence", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
         "params": {"expected_interval_ms": H}},
        {"id": "fallback", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "open_strategy": "breakout", "close_strategies": [NAME]},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=4h"], {"v": 1, "slots": slots})
    assert proc.returncode == 1
    payload = _payload(proc)
    assert payload["error"] == "" and payload["error_scope"] == ""
    results = {r["id"]: r for r in payload["results"]}
    assert sorted(results) == ["cadence", "fallback"]
    assert "does not match timeframe '4h'" in results["cadence"]["error"]
    assert f"close strategy '{NAME}' has invalid parameters" in results["fallback"]["error"]
    assert all(r["signal"] == 0 for r in results.values())
    assert "Fetching BTC" not in proc.stderr
