import importlib.util
import json
import os
import subprocess
import sys

import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "fisher_reversal"
REF = "backtest/candidates/fisher_reversal_1646/REPORT.md"
PARAMS = {"lookback": 9, "extreme": 0.5}
H = 3_600_000
FOUR_H = 4 * H
T0 = 1_699_999_200_000

REFUSED_INPUTS = [
    (["--mode=live"], "explicit live mode"),
    (["--mode", "live"], "explicit live mode"),
    ([], "missing mode"),
    (["--mode=paper", "--mode=paper"], "invalid mode input"),
    (["--mode=paper", "--mode", "live"], "invalid mode input"),
    (["--mode="], "invalid mode input"),
    (["--mode=Paper"], "invalid mode input"),
    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),
    (["--mode=live", "--allow-no-edge=true"], "invalid acknowledgement"),
    (["--mode=paper", "--allow-no-edge", "--allow-no-edge"], "invalid acknowledgement"),
]


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run(
        [sys.executable, CHECK_HL] + argv,
        input="" if stdin is None else json.dumps(stdin),
        capture_output=True, text=True, cwd=REPO, env=_env(), timeout=240,
    )


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _strip(out):
    return {k: v for k, v in out.items() if k not in ("timestamp", "id")}


def _bars(n, step, *, forming_jump=None, start=400.0):
    out = []
    for i in range(n):
        if i < n - 160:
            mid = start + (3.0 if i % 2 == 0 else -2.5)
        else:
            mid = start - 0.5 * (i - (n - 160))
        if forming_jump is not None and i == n - 1:
            mid = out[-1]["c"] + forming_jump
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1,
                    "o": mid, "h": mid + 0.5, "l": mid - 0.5, "c": mid, "v": 1000.0 + i})
    return out


def _rows(bars):
    return [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


def _frame(bars, step, *, timing=True):
    rows = _rows(bars)
    frame = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
             "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
             "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True}
    if timing:
        frame["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": step,
                           "bars": [[b["t"], b["T"], True] for b in bars]}
    return frame


def _market(frames, *, cutoff_ms, mid):
    return {"version": 1, "snapshot_id": "test/1", "generation": 1, "sealed_at_ms": cutoff_ms,
            "decision_cutoff_ms": cutoff_ms, "frames": frames, "feed_complete": True,
            "mids": {"BTC": {"px": mid, "recv_at_ms": cutoff_ms, "source": "ws", "age_ms": 0,
                             "stale": False, "confirmed": True}}}


def _envelope(bars, step, key, **kw):
    cutoff = bars[-1]["t"] + step // 2
    return {"v": 2, "market": _market({key: _frame(bars, step, **kw)}, cutoff_ms=cutoff, mid=bars[-1]["c"])}


def _solo4h():
    return _envelope(_bars(220, FOUR_H), FOUR_H, "BTC|4h")


def _refs(closes=None, params=None):
    refs = {"open": {"name": NAME, "params": dict(params or PARAMS)}}
    if closes is not None:
        refs["closes"] = closes
    return json.dumps(refs)


def _check(bars, *, closed, extra=(), timing=True, step=H):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    proc = _run(argv, _envelope(bars, step, "BTC|1h", timing=timing))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _strategies():
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_fisher_check_futures_strategies", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _frame_df(rows):
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df.set_index("datetime").sort_index()


@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)
def test_fisher_reversal_is_refused_outside_explicit_paper(extra, reason):
    proc = _run([NAME, "BTC", "4h"] + extra)
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert f"open strategy '{NAME}' is edge_status=no_edge" in payload["error"]
    assert REF in payload["error"]
    assert reason in payload["error"]


@pytest.mark.parametrize("refs,mode,needle", [
    ({"open": {"name": NAME, "params": PARAMS}}, ["--mode=live"], f"open strategy '{NAME}' is edge_status=no_edge"),
    ({"open": {"name": NAME, "params": PARAMS}}, [], "missing mode"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=live"],
     f"close strategy '{NAME}' is edge_status=no_edge"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, [], "missing mode"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=live", "--allow-no-edge=yes"],
     "invalid acknowledgement"),
])
def test_effective_open_and_close_fallback_are_gated(refs, mode, needle):
    proc = _run(["breakout", "BTC", "4h", "--strategy-refs", json.dumps(refs), "--market-stdin"] + mode, _solo4h())
    assert proc.returncode == 1
    assert needle in _payload(proc)["error"]


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_paper_or_acknowledged_evaluation_runs_on_sealed_input_without_orders(extra):
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    proc = _run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra, _solo4h())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    assert out["open_strategy"] == NAME and out["close_strategies"] == ["tiered_tp_pct"]
    assert out["indicators"]["fisher_lookback"] == 9.0
    assert out["indicators"]["fisher_valid"] in (0.0, 1.0)
    assert "fill" not in out and "order" not in out


def test_close_fallback_paper_and_acknowledged_live_evaluate():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME, "params": PARAMS}]})
    for extra in (["--mode=paper"], ["--mode=live", "--allow-no-edge"]):
        proc = _run(["breakout", "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra, _solo4h())
        assert proc.returncode == 0, proc.stderr[-2000:]
        assert not _payload(proc).get("error")


def _batch(slots, envelope):
    return _run(["--batch-check", "--symbol=BTC", "--timeframe=4h", "--ohlcv-limit", "200", "--market-stdin"],
                dict(envelope, slots=slots))


def test_batch_slots_apply_the_same_admission_and_match_solo():
    refs = json.loads(_refs([{"name": "tiered_tp_pct", "params": {}}]))
    fallback = {"open": {"name": "breakout"}, "closes": [{"name": NAME}]}
    envelope = _solo4h()
    proc = _batch([
        {"id": "paper", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "acked", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True,
         "strategy_refs": refs},
        {"id": "unacked-live", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs},
        {"id": "missing", "strategy": NAME, "mode": "paper", "strategy_refs": refs},
        {"id": "repeated", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"],
         "strategy_refs": refs},
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
    ], envelope)
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "acked", "fallback-paper"):
        assert not results[ok].get("error"), results[ok]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in results["fallback-live"]["error"]
    solo = _run([NAME, "BTC", "4h", "--mode=paper", "--ohlcv-limit", "200", "--strategy-refs", json.dumps(refs),
                 "--market-stdin"], envelope)
    solo_out = _payload(solo)
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


def test_check_decision_matches_the_evaluator_on_the_200_row_frame():
    bars = _bars(260, H, forming_jump=30.0)
    out = _check(bars, closed=False, extra=["--strategy-refs", _refs()])
    df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[-200:])), PARAMS)
    last = df.iloc[-1]
    assert out["signal"] == int(last["signal"]) == 1 and out["open_action"] == "long"
    for col in ("fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_valid", "fisher_reason_code"):
        assert out["indicators"][col] == round(float(last[col]), 6)


def test_forming_bar_cannot_create_a_closed_bar_entry():
    bars = _bars(260, H, forming_jump=30.0)
    extra = ["--strategy-refs", _refs()]
    disabled = _check(bars, closed=False, extra=extra)
    enabled = _check(bars, closed=True, extra=extra)
    calm = [dict(b) for b in bars]
    calm[-1] = dict(calm[-1], o=bars[-2]["c"], h=bars[-2]["c"] + 9.0, l=bars[-2]["c"] - 0.5, c=bars[-2]["c"] + 8.0)
    calm_out = _check(calm, closed=True, extra=extra)
    assert disabled["signal"] == 1 and disabled["open_action"] == "long"
    assert enabled["signal"] == 0 and enabled["open_action"] == "none"
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert enabled["closed_bar_decision"]["held"] is False
    assert enabled["indicators"] == calm_out["indicators"]
    assert enabled["closed_bar_decision"]["input_sha256"] == calm_out["closed_bar_decision"]["input_sha256"]
    closed_df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[:-1])), PARAMS)
    assert enabled["indicators"]["fisher_reason_code"] == float(closed_df["fisher_reason_code"].iloc[-1]) == 0.0
    assert enabled["indicators"]["fisher_eval_ts_ms"] == float(bars[-2]["T"])


@pytest.mark.parametrize("offset,dropped", [(-1, 1), (0, 0), (1, 0)])
def test_cutoff_at_the_bar_boundary_selects_the_decision_bar(offset, dropped):
    bars = _bars(260, H, forming_jump=30.0)
    boundary = bars[-1]["t"] + H
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", _refs(),
            "--closed-bar-decisions"]
    market = _market({"BTC|1h": _frame(bars, H)}, cutoff_ms=boundary + offset, mid=bars[-1]["c"])
    proc = _run(argv, {"v": 2, "market": market})
    assert proc.returncode == 0, proc.stderr
    out = json.loads(proc.stdout)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped
    assert out["signal"] == (1 if dropped == 0 else 0)


def test_closed_bar_batch_matches_individual_checks():
    bars = _bars(260, H, forming_jump=30.0)
    cutoff = bars[-1]["t"] + H // 2
    market = _market({"BTC|1h": _frame(bars, H)}, cutoff_ms=cutoff, mid=bars[-1]["c"])
    refs = json.loads(_refs())
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                 "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 0, proc.stderr + proc.stdout
    results = {r["id"]: r for r in json.loads(proc.stdout)["results"]}
    for slot_id, closed in (("on", True), ("off", False)):
        argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--atr-method=simple",
                "--strategy-refs", _refs()]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr
        assert _strip(results[slot_id]) == _strip(json.loads(single.stdout))
    assert results["on"]["signal"] == 0
    assert results["off"]["signal"] == 1


HELD_LONG = ["--position-side", "long", "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1"]


@pytest.mark.parametrize("closed", [True, False])
def test_invalid_entry_data_holds_the_entry_and_keeps_protection(closed):
    bars = [dict(b, v=-1.0) for b in _bars(260, H, forming_jump=30.0)]
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    out = _check(bars, closed=closed, extra=["--strategy-refs", refs] + HELD_LONG)
    if closed:
        assert out["closed_bar_decision"]["held"] is True
        assert "negative volume" in out["closed_bar_decision"]["hold_reason"]
        assert out["indicators"] == {}
    else:
        assert out["indicators"]["fisher_reason_code"] == 16.0
        assert out["indicators"]["fisher_valid"] == 0.0
    assert out["open_action"] == "none"
    assert out["close_strategy"] == "tiered_tp_pct" and out["close_fraction"] == 1.0
    assert out["signal"] == -1
    flat = _check(bars, closed=closed, extra=["--strategy-refs", refs])
    assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_missing_timing_holds_the_closed_bar_decision_and_keeps_protection():
    bars = _bars(260, H, forming_jump=30.0)
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    out = _check(bars, closed=True, extra=["--strategy-refs", refs] + HELD_LONG, timing=False)
    assert out["closed_bar_decision"]["held"] is True
    assert out["indicators"] == {}
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=True, extra=["--strategy-refs", refs], timing=False)
    assert flat["signal"] == 0 and flat["closed_bar_decision"]["held"] is True


@pytest.mark.parametrize("closed", [True, False])
def test_missing_decision_frame_is_an_error_without_a_private_fetch(closed):
    bars = _bars(260, H, forming_jump=30.0)
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", _refs()]
    if closed:
        argv.append("--closed-bar-decisions")
    market = _market({"ETH|1h": _frame(bars, H)}, cutoff_ms=bars[-1]["t"] + H // 2, mid=bars[-1]["c"])
    proc = _run(argv, {"v": 2, "market": market})
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0
    assert "BTC|1h" in out["error"]
