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
NAME = "true_strength_index"
REF = "backtest/candidates/true_strength_index_1667/REPORT.md"
PARAMS = {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 18}
BOUNDARY = {"slow_period": 33, "fast_period": 13, "signal_period": 9, "recursion_length": 198}
LADDER = {"tp_tiers": [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
                       {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]}
CLOSE = [{"name": "tiered_tp_pct", "params": LADDER}]
H = 3_600_000
FOUR_H = 4 * H
T0 = 1_699_999_200_000
LONG_SETUP = [399.0, 398.5]
SHORT_SETUP = [401.0, 401.5]
TSI_COLUMNS = ("tsi_value", "tsi_prev_value", "tsi_signal_line", "tsi_prev_signal_line", "tsi_diff", "tsi_prev_diff",
               "tsi_slow_change", "tsi_double_change", "tsi_slow_abs", "tsi_double_abs", "tsi_prev_double_abs",
               "tsi_first_change", "tsi_signal_seed_change", "tsi_close", "tsi_prev_close", "tsi_seed_close",
               "tsi_prev_seed_close", "tsi_valid", "tsi_value_valid", "tsi_reason_code", "tsi_required_history")
HELD_LONG = ["--position-side", "long", "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1"]
HELD_SHORT = ["--position-side", "short", "--position-avg-cost=4000", "--position-qty=1", "--position-initial-qty=1"]


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


def _bars(n, step, tail, start=400.0):
    closes = [start] * (n - len(tail)) + list(tail)
    out = []
    prev = start
    for i, c in enumerate(closes):
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1, "o": prev, "h": max(prev, c) + 0.5,
                    "l": min(prev, c) - 0.5, "c": c, "v": 1000.0 + i})
        prev = c
    return out


def _long(n=260, step=H, jump=8.0):
    return _bars(n, step, LONG_SETUP + [LONG_SETUP[-1] + jump])


def _short(n=260, step=H, jump=8.0):
    return _bars(n, step, SHORT_SETUP + [SHORT_SETUP[-1] - jump])


def _rows(bars):
    return [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


def _frame(bars, step, *, timing=True, rows=None):
    rows = _rows(bars) if rows is None else rows
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


def _refs(closes=None, params=None):
    refs = {"open": {"name": NAME, "params": dict(PARAMS if params is None else params)}}
    if closes is not None:
        refs["closes"] = closes
    return json.dumps(refs)


def _check(bars, *, closed, extra=(), timing=True, step=H, cutoff=None, limit=200, rows=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", str(limit), *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    env = _envelope(bars, step, "BTC|1h", timing=timing, rows=rows)
    if cutoff is not None:
        env["market"]["decision_cutoff_ms"] = cutoff
        env["market"]["sealed_at_ms"] = cutoff
    proc = _run(argv, env)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _strategies():
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_tsi_check_futures_strategies", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _frame_df(rows, kind=None):
    from closed_bar import declare_frame_cadence
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return declare_frame_cadence(df.set_index("datetime").sort_index(), "1h", kind)


def test_candidate_without_mode_is_refused():
    proc = _run([NAME, "BTC", "4h"])
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert payload["strategy"] == NAME
    assert f"open strategy '{NAME}' is edge_status=no_edge" in payload["error"]
    assert REF in payload["error"]
    assert "missing mode" in payload["error"]


def test_close_fallback_reference_is_gated_separately():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})
    proc = _run(["breakout", "BTC", "4h", "--strategy-refs", refs])
    assert proc.returncode == 1, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["signal"] == 0
    assert f"close strategy '{NAME}' is edge_status=no_edge" in out["error"]
    assert "missing mode" in out["error"]
    env = _envelope(_long(220, FOUR_H), FOUR_H, "BTC|4h")
    paper = _run(["breakout", "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--mode=paper"], env)
    assert paper.returncode == 0, paper.stderr[-2000:]
    assert not _payload(paper).get("error")


def test_explicit_paper_evaluation_runs_both_directions_on_sealed_input_without_orders():
    refs = _refs(CLOSE)
    env = _envelope(_long(220, FOUR_H), FOUR_H, "BTC|4h")
    proc = _run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--mode=paper"], env)
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    assert out["open_strategy"] == NAME and out["close_strategies"] == ["tiered_tp_pct"]
    assert out["indicators"]["tsi_recursion_length"] == 18.0
    assert out["indicators"]["tsi_required_history"] == 19.0
    assert out["indicators"]["tsi_valid"] == 1.0
    assert out["indicator_labels"]["tsi_reason"] == "entry_long"
    assert out["signal"] == 1 and out["open_action"] == "long"
    assert "fill" not in out and "order" not in out
    env = _envelope(_short(220, FOUR_H), FOUR_H, "BTC|4h")
    short = _payload(_run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--mode=paper"], env))
    assert short["signal"] == -1 and short["open_action"] == "short"
    assert short["indicator_labels"]["tsi_reason"] == "entry_short"


@pytest.mark.parametrize("params", [
    {**PARAMS, "recursion_length": 17}, {**PARAMS, "recursion_length": 199, "slow_period": 3},
    {**PARAMS, "signal_period": 18}, {**PARAMS, "slow_period": True}, {**PARAMS, "fast_period": 3},
])
def test_invalid_parameters_fail_preflight_before_market_input_is_read(params):
    market = _market({"ETH|4h": _frame(_long(220, FOUR_H), FOUR_H)}, cutoff_ms=T0, mid=400.0)
    proc = _run([NAME, "BTC", "4h", "--mode=paper", "--strategy-refs", _refs(params=params), "--market-stdin"],
                {"v": 2, "market": market})
    assert proc.returncode == 1
    err = _payload(proc)["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in err
    assert "BTC|4h" not in err


def test_check_decision_matches_the_evaluator_on_the_200_row_frame_at_full_precision():
    bars = _long()
    out = _check(bars, closed=False, extra=["--strategy-refs", _refs()])
    df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[-200:])), PARAMS)
    full = _strategies().apply_strategy(NAME, _frame_df(_rows(bars)), PARAMS)
    last = df.iloc[-1]
    assert out["signal"] == int(last["signal"]) == 1 and out["open_action"] == "long"
    for col in TSI_COLUMNS:
        assert out["indicators"][col] == float(last[col]) == float(full[col].iloc[-1]), col
    assert out["indicator_labels"]["tsi_reason"] == last["tsi_reason"] == "entry_long"
    ind = out["indicators"]
    assert ind["tsi_diff"] == ind["tsi_value"] - ind["tsi_signal_line"] > 0
    assert ind["tsi_prev_diff"] == 0.0 and ind["tsi_value"] > 0
    assert ind["tsi_value"] == 100.0 * (ind["tsi_double_change"] / ind["tsi_double_abs"])
    assert ind["tsi_value"] != round(ind["tsi_value"], 6)


def test_forming_bar_cannot_create_a_closed_bar_entry():
    bars = _long()
    extra = ["--strategy-refs", _refs()]
    disabled = _check(bars, closed=False, extra=extra)
    enabled = _check(bars, closed=True, extra=extra)
    calm = [dict(b) for b in bars]
    calm[-1] = dict(calm[-1], h=bars[-2]["c"] + 30.0, l=bars[-2]["c"] - 30.0, c=bars[-2]["c"] - 20.0)
    calm_out = _check(calm, closed=True, extra=extra)
    assert disabled["signal"] == 1 and disabled["open_action"] == "long"
    assert enabled["signal"] == 0 and enabled["open_action"] == "none"
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert enabled["closed_bar_decision"]["held"] is False
    assert enabled["indicators"] == calm_out["indicators"]
    assert enabled["closed_bar_decision"]["input_sha256"] == calm_out["closed_bar_decision"]["input_sha256"]
    closed_df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[:-1]), "native_close_inclusive"), PARAMS)
    assert enabled["indicators"]["tsi_reason_code"] == float(closed_df["tsi_reason_code"].iloc[-1]) == 0.0
    assert enabled["indicator_labels"]["tsi_reason"] == "no_cross"
    assert enabled["indicators"]["tsi_eval_ts_ms"] == float(bars[-2]["t"])


def test_an_already_closed_final_bar_is_the_decision_bar():
    bars = _long()
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=bars[-1]["t"] + H)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
    assert out["signal"] == 1 and out["indicators"]["tsi_eval_ts_ms"] == float(bars[-1]["t"])


@pytest.mark.parametrize("offset,dropped", [(-1, 1), (0, 0), (1, 0)])
def test_cutoff_at_the_bar_boundary_selects_the_decision_bar(offset, dropped):
    bars = _long()
    boundary = bars[-1]["t"] + H
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=boundary + offset)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped
    assert out["signal"] == (1 if dropped == 0 else 0)


@pytest.mark.parametrize("rows,closed_reason", [(40, "insufficient_history"), (41, "entry_long")])
def test_selected_closed_history_needs_w_plus_one_bars(rows, closed_reason):
    params = {**PARAMS, "recursion_length": 40}
    bars = _bars(rows + 1, H, LONG_SETUP + [406.5, 410.0])
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs(params=params)])
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["indicator_labels"]["tsi_reason"] == closed_reason
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


@pytest.mark.parametrize("limit,closed_reason", [(200, "entry_long"), (199, "entry_long"),
                                                 (198, "insufficient_history")])
def test_the_ordinary_200_row_window_carries_the_largest_support(limit, closed_reason):
    bars = _bars(260, H, LONG_SETUP + [406.5, 410.0])
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs(params=BOUNDARY)], limit=limit)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["closed_bar_decision"]["rows"] == limit
    assert out["indicator_labels"]["tsi_reason"] == closed_reason
    assert out["indicators"]["tsi_required_history"] == 199.0
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


def test_closed_bar_batch_matches_individual_checks_and_isolates_a_failing_slot():
    bars = _long()
    cutoff = bars[-1]["t"] + H // 2
    market = _market({"BTC|1h": _frame(bars, H)}, cutoff_ms=cutoff, mid=bars[-1]["c"])
    refs = json.loads(_refs())
    bad = json.loads(_refs(params={**PARAMS, "recursion_length": 17}))
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "refused", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "bad-params", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": bad,
         "closed_bar_decisions": True},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                 "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert "explicit live mode" in results["refused"]["error"] and results["refused"]["signal"] == 0
    assert "has invalid parameters" in results["bad-params"]["error"] and results["bad-params"]["signal"] == 0
    assert not results["refused"].get("indicators") and not results["bad-params"].get("indicators")
    for slot_id, closed in (("on", True), ("off", False)):
        assert not results[slot_id].get("error"), results[slot_id]
        argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--atr-method=simple",
                "--strategy-refs", _refs()]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr
        assert _strip(results[slot_id]) == _strip(json.loads(single.stdout))
    assert results["on"]["signal"] == 0
    assert results["off"]["signal"] == 1


@pytest.mark.parametrize("closed", [True, False])
@pytest.mark.parametrize("held,close_signal", [(HELD_LONG, -1), (HELD_SHORT, 1)])
def test_invalid_entry_data_holds_the_entry_and_keeps_protection(closed, held, close_signal):
    bars = _long()
    bars[-5] = dict(bars[-5], h=bars[-5]["l"] - 1.0)
    refs = _refs(CLOSE)
    out = _check(bars, closed=closed, extra=["--strategy-refs", refs] + held)
    if closed:
        assert out["closed_bar_decision"]["held"] is True
        assert out["indicators"] == {}
    else:
        assert out["indicator_labels"]["tsi_reason"] == "invalid_candle_geometry"
        assert out["indicators"]["tsi_valid"] == 0.0
    assert out["open_action"] == "none"
    assert out["close_strategy"] == "tiered_tp_pct" and out["close_fraction"] == 1.0
    assert out["signal"] == close_signal
    flat = _check(bars, closed=closed, extra=["--strategy-refs", refs])
    assert flat["signal"] == 0 and flat["open_action"] == "none"


@pytest.mark.parametrize("held,close_signal", [(HELD_LONG, -1), (HELD_SHORT, 1)])
def test_cadence_gap_holds_the_entry_and_keeps_protection(held, close_signal):
    bars = _long()
    del bars[-10]
    refs = _refs(CLOSE)
    out = _check(bars, closed=False, extra=["--strategy-refs", refs] + held, timing=False)
    assert out["indicator_labels"]["tsi_reason"] == "cadence_gap"
    assert out["open_action"] == "none" and out["close_fraction"] == 1.0 and out["signal"] == close_signal
    flat = _check(bars, closed=False, extra=["--strategy-refs", refs], timing=False)
    assert flat["signal"] == 0 and flat["indicator_labels"]["tsi_reason"] == "cadence_gap"


def test_received_order_defects_survive_normalization_and_hold_the_entry():
    bars = _long()
    rows = _rows(bars)
    rows[-6], rows[-7] = rows[-7], rows[-6]
    refs = _refs(CLOSE)
    out = _check(bars, closed=False, extra=["--strategy-refs", refs] + HELD_LONG, timing=False, rows=rows)
    assert out["indicator_labels"]["tsi_reason"] == "timestamp_order"
    assert out["indicators"]["tsi_valid"] == 0.0
    assert out["open_action"] == "none" and out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=False, extra=["--strategy-refs", refs], timing=False, rows=rows)
    assert flat["signal"] == 0 and flat["indicator_labels"]["tsi_reason"] == "timestamp_order"
    clean = _check(bars, closed=False, extra=["--strategy-refs", refs], timing=False)
    assert clean["signal"] == 1


def test_missing_timing_holds_the_closed_bar_decision_and_keeps_protection():
    bars = _long()
    refs = _refs(CLOSE)
    out = _check(bars, closed=True, extra=["--strategy-refs", refs] + HELD_LONG, timing=False)
    assert out["closed_bar_decision"]["held"] is True
    assert out["indicators"] == {}
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=True, extra=["--strategy-refs", refs], timing=False)
    assert flat["signal"] == 0 and flat["closed_bar_decision"]["held"] is True


@pytest.mark.parametrize("closed", [True, False])
def test_missing_decision_frame_is_an_error_without_a_private_fetch(closed):
    bars = _long()
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", _refs()]
    if closed:
        argv.append("--closed-bar-decisions")
    market = _market({"ETH|1h": _frame(bars, H)}, cutoff_ms=bars[-1]["t"] + H // 2, mid=bars[-1]["c"])
    proc = _run(argv, {"v": 2, "market": market})
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0
    assert "BTC|1h" in out["error"]
