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
NAME = "trix_momentum"
REF = "backtest/candidates/trix_momentum_1659/REPORT.md"
PARAMS = {"period": 3, "signal_period": 4, "recursion_length": 20}
LADDER = {"tp_tiers": [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
                       {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]}
CLOSE = [{"name": "tiered_tp_pct", "params": LADDER}]
H = 3_600_000
FOUR_H = 4 * H
T0 = 1_699_999_200_000
TRIX_COLUMNS = ("trix_value", "trix_prev_value", "trix_signal_line", "trix_prev_signal_line", "trix_diff",
                "trix_prev_diff", "trix_ema3", "trix_ema3_base", "trix_prev_ema3", "trix_prev_ema3_base",
                "trix_close", "trix_prev_close", "trix_seed_close", "trix_prev_seed_close", "trix_valid",
                "trix_value_valid", "trix_reason_code", "trix_required_history")
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


def _bars(n, step, *, forming_jump=None, start=400.0):
    out = []
    for i in range(n):
        mid = start
        if forming_jump is not None and i == n - 1:
            mid = start + forming_jump
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1,
                    "o": start, "h": max(start, mid) + 0.5, "l": min(start, mid) - 0.5, "c": mid, "v": 1000.0 + i})
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
    return {"version": 4, "snapshot_id": "test/1", "generation": 1, "sealed_at_ms": cutoff_ms,
            "decision_cutoff_ms": cutoff_ms, "frames": frames, "feed_complete": True,
            "mids": {"BTC": {"px": mid, "recv_at_ms": cutoff_ms, "source": "ws", "age_ms": 0,
                             "stale": False, "confirmed": True}}}


def _envelope(bars, step, key, **kw):
    cutoff = bars[-1]["t"] + step // 2
    return {"v": 2, "market": _market({key: _frame(bars, step, **kw)}, cutoff_ms=cutoff, mid=bars[-1]["c"])}


def _solo4h(jump=8.0):
    return _envelope(_bars(220, FOUR_H, forming_jump=jump), FOUR_H, "BTC|4h")


def _refs(closes=None, params=None):
    refs = {"open": {"name": NAME, "params": dict(PARAMS if params is None else params)}}
    if closes is not None:
        refs["closes"] = closes
    return json.dumps(refs)


def _check(bars, *, closed, extra=(), timing=True, step=H, cutoff=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    env = _envelope(bars, step, "BTC|1h", timing=timing)
    if cutoff is not None:
        env["market"]["decision_cutoff_ms"] = cutoff
        env["market"]["sealed_at_ms"] = cutoff
    proc = _run(argv, env)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _strategies():
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_trix_check_futures_strategies", path)
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
    live = _run(["breakout", "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--mode=live"], _solo4h())
    assert live.returncode == 1
    out = _payload(live)
    assert out["signal"] == 0
    assert f"close strategy '{NAME}' is edge_status=no_edge" in out["error"]
    assert "explicit live mode" in out["error"]
    for extra in (["--mode=paper"], ["--mode=live", "--allow-no-edge"]):
        proc = _run(["breakout", "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra, _solo4h())
        assert proc.returncode == 0, proc.stderr[-2000:]
        assert not _payload(proc).get("error")


def test_paper_and_acknowledged_evaluation_run_on_sealed_input_without_orders():
    refs = _refs(CLOSE)
    outs = []
    for extra in (["--mode=paper"], ["--mode=live", "--allow-no-edge"]):
        proc = _run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra, _solo4h())
        assert proc.returncode == 0, proc.stderr[-2000:]
        out = _payload(proc)
        assert not out.get("error")
        assert out["open_strategy"] == NAME and out["close_strategies"] == ["tiered_tp_pct"]
        assert out["indicators"]["trix_recursion_length"] == 20.0
        assert out["indicators"]["trix_valid"] == 1.0
        assert out["indicator_labels"]["trix_reason"] == "entry_long"
        assert out["signal"] == 1 and out["open_action"] == "long"
        assert "fill" not in out and "order" not in out
        outs.append(out)
    assert outs[0]["indicators"] == outs[1]["indicators"]
    short = _payload(_run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--mode=paper"],
                          _solo4h(-8.0)))
    assert short["signal"] == -1 and short["open_action"] == "short"
    assert short["indicator_labels"]["trix_reason"] == "entry_short"


@pytest.mark.parametrize("params", [
    {**PARAMS, "recursion_length": 17}, {**PARAMS, "recursion_length": 198, "period": 3},
    {**PARAMS, "signal_period": 20}, {**PARAMS, "period": True},
])
def test_invalid_parameters_fail_preflight_before_market_input_is_read(params):
    market = _market({"ETH|4h": _frame(_bars(220, FOUR_H), FOUR_H)}, cutoff_ms=T0, mid=400.0)
    proc = _run([NAME, "BTC", "4h", "--mode=paper", "--strategy-refs", _refs(params=params), "--market-stdin"],
                {"v": 2, "market": market})
    assert proc.returncode == 1
    err = _payload(proc)["error"]
    assert f"open strategy '{NAME}' has invalid parameters" in err
    assert "BTC|4h" not in err


def _batch(slots, envelope, timeframe="4h"):
    return _run(["--batch-check", "--symbol=BTC", f"--timeframe={timeframe}", "--ohlcv-limit", "200",
                 "--market-stdin"], dict(envelope, slots=slots))


def test_batch_slots_apply_the_same_admission_and_match_solo():
    refs = json.loads(_refs(CLOSE))
    bad = json.loads(_refs(CLOSE, params={**PARAMS, "recursion_length": 17}))
    fallback = {"open": {"name": "breakout"}, "closes": [{"name": NAME}]}
    envelope = _solo4h()
    proc = _batch([
        {"id": "paper", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "acked", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True,
         "strategy_refs": refs},
        {"id": "unacked-live", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs},
        {"id": "missing", "strategy": NAME, "mode": "paper", "strategy_refs": refs},
        {"id": "bad-params", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": bad},
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
    ], envelope)
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "acked", "fallback-paper"):
        assert not results[ok].get("error"), results[ok]
    for refused in ("unacked-live", "missing", "bad-params", "fallback-live"):
        assert results[refused]["signal"] == 0 and results[refused].get("error")
        assert not results[refused].get("indicators")
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "has invalid parameters" in results["bad-params"]["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in results["fallback-live"]["error"]
    solo = _run([NAME, "BTC", "4h", "--mode=paper", "--ohlcv-limit", "200", "--strategy-refs", json.dumps(refs),
                 "--market-stdin"], envelope)
    solo_out = _payload(solo)
    assert solo_out["signal"] == 1
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


def test_check_decision_matches_the_evaluator_on_the_200_row_frame_at_full_precision():
    bars = _bars(260, H, forming_jump=8.0)
    out = _check(bars, closed=False, extra=["--strategy-refs", _refs()])
    df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[-200:])), PARAMS)
    full = _strategies().apply_strategy(NAME, _frame_df(_rows(bars)), PARAMS)
    last = df.iloc[-1]
    assert out["signal"] == int(last["signal"]) == 1 and out["open_action"] == "long"
    for col in TRIX_COLUMNS:
        assert out["indicators"][col] == float(last[col]) == float(full[col].iloc[-1])
    assert out["indicator_labels"]["trix_reason"] == last["trix_reason"] == "entry_long"
    ind = out["indicators"]
    assert ind["trix_diff"] == ind["trix_value"] - ind["trix_signal_line"] > 0
    assert ind["trix_prev_diff"] <= 0 and ind["trix_value"] > 0
    assert ind["trix_value"] == 100.0 * (ind["trix_ema3"] - ind["trix_ema3_base"]) / ind["trix_ema3_base"]


def test_forming_bar_cannot_create_a_closed_bar_entry():
    bars = _bars(260, H, forming_jump=8.0)
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
    assert enabled["indicators"]["trix_reason_code"] == float(closed_df["trix_reason_code"].iloc[-1]) == 0.0
    assert enabled["indicator_labels"]["trix_reason"] == "no_cross"
    assert enabled["indicators"]["trix_eval_ts_ms"] == float(bars[-2]["t"])


def test_an_already_closed_final_bar_is_the_decision_bar():
    bars = _bars(260, H, forming_jump=8.0)
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=bars[-1]["t"] + H)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
    assert out["signal"] == 1 and out["indicators"]["trix_eval_ts_ms"] == float(bars[-1]["t"])


@pytest.mark.parametrize("offset,dropped", [(-1, 1), (0, 0), (1, 0)])
def test_cutoff_at_the_bar_boundary_selects_the_decision_bar(offset, dropped):
    bars = _bars(260, H, forming_jump=8.0)
    boundary = bars[-1]["t"] + H
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=boundary + offset)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped
    assert out["signal"] == (1 if dropped == 0 else 0)


@pytest.mark.parametrize("rows,closed_reason", [(41, "insufficient_history"), (42, "entry_long")])
def test_selected_closed_history_needs_l_plus_two_bars(rows, closed_reason):
    params = {**PARAMS, "recursion_length": 40}
    bars = _bars(rows + 1, H, forming_jump=None)
    bars[-2] = dict(bars[-2], h=409.0, c=408.0)
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs(params=params)])
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["indicator_labels"]["trix_reason"] == closed_reason
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


@pytest.mark.parametrize("limit,closed_reason", [(200, "entry_long"), (198, "insufficient_history")])
def test_the_ordinary_200_row_window_carries_the_largest_dependency(limit, closed_reason):
    params = {"period": 32, "signal_period": 9, "recursion_length": 197}
    bars = _bars(260, H, forming_jump=None)
    bars[-2] = dict(bars[-2], h=409.0, c=408.0)
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", str(limit), "--strategy-refs",
            _refs(params=params), "--closed-bar-decisions"]
    proc = _run(argv, _envelope(bars, H, "BTC|1h"))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["closed_bar_decision"]["rows"] == limit
    assert out["indicator_labels"]["trix_reason"] == closed_reason
    assert out["indicators"]["trix_required_history"] == 199.0
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


def test_closed_bar_batch_matches_individual_checks():
    bars = _bars(260, H, forming_jump=8.0)
    cutoff = bars[-1]["t"] + H // 2
    market = _market({"BTC|1h": _frame(bars, H)}, cutoff_ms=cutoff, mid=bars[-1]["c"])
    refs = json.loads(_refs())
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "refused", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs,
         "closed_bar_decisions": True},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                 "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert "explicit live mode" in results["refused"]["error"] and results["refused"]["signal"] == 0
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


@pytest.mark.parametrize("closed", [True, False])
@pytest.mark.parametrize("held,close_signal", [(HELD_LONG, -1), (HELD_SHORT, 1)])
def test_invalid_entry_data_holds_the_entry_and_keeps_protection(closed, held, close_signal):
    bars = _bars(260, H, forming_jump=8.0)
    bars[-5] = dict(bars[-5], h=bars[-5]["l"] - 1.0)
    refs = _refs(CLOSE)
    out = _check(bars, closed=closed, extra=["--strategy-refs", refs] + held)
    if closed:
        assert out["closed_bar_decision"]["held"] is True
        assert out["indicators"] == {}
    else:
        assert out["indicator_labels"]["trix_reason"] == "invalid_candle_geometry"
        assert out["indicators"]["trix_valid"] == 0.0
    assert out["open_action"] == "none"
    assert out["close_strategy"] == "tiered_tp_pct" and out["close_fraction"] == 1.0
    assert out["signal"] == close_signal
    flat = _check(bars, closed=closed, extra=["--strategy-refs", refs])
    assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_cadence_gap_holds_the_entry_and_keeps_protection():
    bars = _bars(260, H, forming_jump=8.0)
    del bars[-10]
    refs = _refs(CLOSE)
    out = _check(bars, closed=False, extra=["--strategy-refs", refs] + HELD_LONG, timing=False)
    assert out["indicator_labels"]["trix_reason"] == "cadence_gap"
    assert out["open_action"] == "none" and out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=False, extra=["--strategy-refs", refs], timing=False)
    assert flat["signal"] == 0 and flat["indicator_labels"]["trix_reason"] == "cadence_gap"


def test_missing_timing_holds_the_closed_bar_decision_and_keeps_protection():
    bars = _bars(260, H, forming_jump=8.0)
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
    bars = _bars(260, H, forming_jump=8.0)
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", _refs()]
    if closed:
        argv.append("--closed-bar-decisions")
    market = _market({"ETH|1h": _frame(bars, H)}, cutoff_ms=bars[-1]["t"] + H // 2, mid=bars[-1]["c"])
    proc = _run(argv, {"v": 2, "market": market})
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0
    assert "BTC|1h" in out["error"]
