import functools
import importlib.util
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
NAME = "schaff_trend_cycle"
REF = "backtest/candidates/schaff_trend_cycle_1651/REPORT.md"
PARAMS = {"fast_period": 23, "slow_period": 50, "cycle_length": 10, "smoothing": 0.5,
          "lower_threshold": 25.0, "upper_threshold": 75.0}
H = 3_600_000
FOUR_H = 4 * H
T0 = 1_699_999_200_000

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


def _walk_bars(n, step, seed=3, start=400.0):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.01, n)))
    out = []
    prev = start
    for i in range(n):
        c = float(close[i])
        o = float(prev)
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1, "o": o, "h": max(o, c) * 1.002,
                    "l": min(o, c) * 0.998, "c": c, "v": 1000.0 + i})
        prev = c
    return out


def _rows(bars):
    return [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


def _strategies():
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_stc_check_futures_strategies", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _frame_df(rows, step=H):
    from closed_bar import TIMESTAMP_KIND_NATIVE_CLOSE, declare_frame_cadence
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime").sort_index()
    return declare_frame_cadence(df, "1h" if step == H else "4h", TIMESTAMP_KIND_NATIVE_CLOSE)


@functools.lru_cache(maxsize=None)
def _entry_index(side, step=H):
    bars = _walk_bars(1200, step)
    out = _strategies().apply_strategy(NAME, _frame_df(_rows(bars), step), PARAMS)
    sig = out["signal"].to_numpy()
    k = next(int(i) for i in np.flatnonzero(sig == side) if i >= 260 and sig[i - 1] == 0)
    return k


def _entry_bars(side=1, step=H):
    return _walk_bars(1200, step)[:_entry_index(side, step) + 1]


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
    return {"version": 2, "snapshot_id": "test/1", "generation": 1, "sealed_at_ms": cutoff_ms,
            "decision_cutoff_ms": cutoff_ms, "frames": frames, "feed_complete": True,
            "mids": {"BTC": {"px": mid, "recv_at_ms": cutoff_ms, "source": "ws", "age_ms": 0,
                             "stale": False, "confirmed": True}}}


def _envelope(bars, step, key, *, cutoff=None, **kw):
    if cutoff is None:
        cutoff = bars[-1]["t"] + step // 2
    return {"v": 2, "market": _market({key: _frame(bars, step, **kw)}, cutoff_ms=cutoff, mid=bars[-1]["c"])}


def _solo4h():
    return _envelope(_walk_bars(240, FOUR_H), FOUR_H, "BTC|4h")


def _refs(closes=None, params=None):
    refs = {"open": {"name": NAME, "params": dict(params or PARAMS)}}
    if closes is not None:
        refs["closes"] = closes
    return json.dumps(refs)


def _check(bars, *, closed, extra=(), timing=True, step=H, cutoff=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    proc = _run(argv, _envelope(bars, step, "BTC|1h", timing=timing, cutoff=cutoff))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    return json.loads(proc.stdout)


def _reconstruct(ind):
    lower, upper = ind["stc_lower_threshold"], ind["stc_upper_threshold"]
    if ind.get("stc_valid") != 1.0:
        return 0
    if ind["stc_prev_value"] <= lower < ind["stc_value"] and ind["stc_macd"] > ind["stc_prev_macd"]:
        return 1
    if ind["stc_prev_value"] >= upper > ind["stc_value"] and ind["stc_macd"] < ind["stc_prev_macd"]:
        return -1
    return 0


def test_schaff_without_mode_is_refused():
    proc = _run([NAME, "BTC", "4h"])
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert payload["strategy"] == NAME
    assert f"open strategy '{NAME}' is edge_status=no_edge" in payload["error"]
    assert REF in payload["error"]
    assert "missing mode" in payload["error"]


@pytest.mark.parametrize("refs,mode,needle", [
    ({"open": {"name": NAME, "params": PARAMS}}, ["--mode=live"], f"open strategy '{NAME}' is edge_status=no_edge"),
    ({"open": {"name": NAME, "params": PARAMS}}, [], "missing mode"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=live"],
     f"close strategy '{NAME}' is edge_status=no_edge"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, [], "missing mode"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=live", "--allow-no-edge=yes"],
     "invalid acknowledgement"),
    ({"open": {"name": "breakout"}, "closes": [{"name": NAME}]}, ["--mode=paper", "--mode=paper"],
     "invalid mode input"),
])
def test_effective_open_and_close_fallback_are_gated(refs, mode, needle):
    proc = _run(["breakout", "BTC", "4h", "--strategy-refs", json.dumps(refs), "--market-stdin"] + mode, _solo4h())
    assert proc.returncode == 1
    assert needle in _payload(proc)["error"]


@pytest.mark.parametrize("params,needle", [
    (dict(PARAMS, fast_period=50), "fast_period"),
    (dict(PARAMS, cycle_length=1.5), "cycle_length"),
    (dict(PARAMS, lower_threshold=80.0), "lower_threshold"),
    (dict(PARAMS, smoothing=True), "smoothing"),
])
def test_invalid_parameters_are_preflight_errors(params, needle):
    proc = _run([NAME, "BTC", "4h", "--mode=paper", "--strategy-refs", _refs(params=params), "--market-stdin"],
                _solo4h())
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0 and needle in out["error"]
    fallback = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME, "params": params}]})
    proc = _run(["breakout", "BTC", "4h", "--mode=paper", "--strategy-refs", fallback, "--market-stdin"], _solo4h())
    assert proc.returncode == 1 and needle in _payload(proc)["error"]


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_paper_or_acknowledged_evaluation_runs_on_sealed_input_without_orders(extra):
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    proc = _run([NAME, "BTC", "4h", "--strategy-refs", refs, "--market-stdin", "--closed-bar-decisions"] + extra,
                _solo4h())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    assert out["open_strategy"] == NAME and out["close_strategies"] == ["tiered_tp_pct"]
    assert out["indicators"]["stc_fast_period"] == 23.0
    assert out["indicators"]["stc_required_history"] == 193.0
    assert out["indicator_labels"]["stc_reason"]
    assert "fill" not in out and "order" not in out


def test_close_fallback_paper_and_acknowledged_live_evaluate():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME, "params": PARAMS}]})
    for extra in (["--mode=paper"], ["--mode=live", "--allow-no-edge"]):
        proc = _run(["breakout", "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra, _solo4h())
        assert proc.returncode == 0, proc.stderr[-2000:]
        assert not _payload(proc).get("error")


def test_retained_strategies_still_import_and_evaluate():
    for name in ("breakout", "macd", "stoch_rsi", "fisher_reversal"):
        proc = _run([name, "BTC", "4h", "--mode=paper", "--market-stdin"], _solo4h())
        assert proc.returncode == 0, (name, proc.stderr[-2000:])
        assert not _payload(proc).get("error")


def _batch(slots, envelope, timeframe="4h"):
    return _run(["--batch-check", "--symbol=BTC", f"--timeframe={timeframe}", "--ohlcv-limit", "200",
                 "--market-stdin"], dict(envelope, slots=slots))


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
        {"id": "fallback-missing", "strategy": "breakout", "mode": "paper", "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
        {"id": "fallback-acked", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True, "strategy_refs": fallback},
    ], envelope)
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "acked", "fallback-paper", "fallback-acked"):
        assert not results[ok].get("error"), results[ok]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in results["fallback-live"]["error"]
    assert "missing mode" in results["fallback-missing"]["error"]
    solo = _run([NAME, "BTC", "4h", "--mode=paper", "--ohlcv-limit", "200", "--strategy-refs", json.dumps(refs),
                 "--market-stdin"], envelope)
    solo_out = _payload(solo)
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


def test_batch_refuses_a_malformed_acknowledgement():
    refs = json.loads(_refs())
    proc = _batch([{"id": "bad-ack", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"],
                    "allow_no_edge": "yes", "strategy_refs": refs}], _solo4h())
    assert proc.returncode == 1
    out = _payload(proc)
    assert "JSON boolean" in json.dumps(out)
    assert all(r.get("signal", 0) == 0 for r in out.get("results", []))


@pytest.mark.parametrize("side", [1, -1])
def test_closed_bar_entry_reproduces_from_the_emitted_values(side):
    bars = _entry_bars(side)
    boundary = bars[-1]["T"] + 1
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=boundary)
    assert out["closed_bar_decision"]["held"] is False
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
    ind = out["indicators"]
    assert out["signal"] == side == _reconstruct(ind)
    assert out["indicator_labels"]["stc_reason"] == ("entry_long" if side == 1 else "entry_short")
    assert ind["stc_decision_cutoff_ms"] == float(boundary) and ind["stc_decision_boundary_ms"] == float(boundary)
    df = _strategies().apply_strategy(NAME, _frame_df(_rows(bars[-200:])), PARAMS)
    for col in ("stc_value", "stc_prev_value", "stc_macd", "stc_prev_macd", "stc_valid", "stc_reason_code"):
        assert ind[col] == float(df[col].iloc[-1])
    key = "lower_threshold" if side == 1 else "upper_threshold"
    at_prev = dict(PARAMS, **{key: ind["stc_prev_value"]})
    at_cur = dict(PARAMS, **{key: ind["stc_value"]})
    for params, want in ((at_prev, side), (at_cur, 0)):
        boundary_out = _check(bars, closed=True, extra=["--strategy-refs", _refs(params=params)], cutoff=boundary)
        assert boundary_out["signal"] == want == _reconstruct(boundary_out["indicators"])


@pytest.mark.parametrize("offset,dropped,signal,reason", [
    (-1, 1, 0, None),
    (0, 0, 1, "entry_long"),
    (1, 0, 1, "entry_long"),
    (H - 1, 0, 1, "entry_long"),
    (H, 0, 0, "decision_bar_stale"),
    (5 * H, 0, 0, "decision_bar_stale"),
])
def test_cutoff_boundaries_select_and_age_the_decision_bar(offset, dropped, signal, reason):
    bars = _entry_bars(1)
    boundary = bars[-1]["T"] + 1
    out = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=boundary + offset)
    assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped
    assert out["signal"] == signal
    if reason:
        assert out["indicator_labels"]["stc_reason"] == reason


def test_forming_candle_that_would_create_an_entry_is_excluded():
    bars = _entry_bars(1)
    forming = _check(bars, closed=True, extra=["--strategy-refs", _refs()], cutoff=bars[-1]["t"] + H // 2)
    closed = _check(bars[:-1], closed=True, extra=["--strategy-refs", _refs()], cutoff=bars[-1]["t"] + H // 2)
    assert forming["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert forming["signal"] == 0 and forming["open_action"] == "none"
    assert forming["indicators"] == closed["indicators"]
    assert forming["closed_bar_decision"]["input_sha256"] == closed["closed_bar_decision"]["input_sha256"]
    untimed = _check(bars, closed=False, extra=["--strategy-refs", _refs()])
    assert untimed["signal"] == 0
    assert untimed["indicator_labels"]["stc_reason"] == "decision_time_missing"


def test_closed_bar_batch_matches_individual_checks():
    bars = _entry_bars(1)
    market = _market({"BTC|1h": _frame(bars, H)}, cutoff_ms=bars[-1]["T"] + 1, mid=bars[-1]["c"])
    refs = json.loads(_refs())
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs,
         "closed_bar_decisions": True},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "peer", "strategy": "macd", "mode": "paper", "mode_args": ["--mode=paper"],
         "closed_bar_decisions": True},
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
    assert results["on"]["signal"] == 1 == _reconstruct(results["on"]["indicators"])
    assert results["off"]["signal"] == 0
    assert not results["peer"].get("error")


HELD_LONG = ["--position-side", "long", "--position-avg-cost=50", "--position-qty=1", "--position-initial-qty=1"]
HELD_SHORT = ["--position-side", "short", "--position-avg-cost=5000", "--position-qty=1",
              "--position-initial-qty=1"]


@pytest.mark.parametrize("entry_side,held,close_signal", [(-1, HELD_LONG, -1), (1, HELD_SHORT, 1)])
def test_closed_bar_close_fallback_closes_on_an_opposite_entry(entry_side, held, close_signal):
    bars = _entry_bars(entry_side)
    boundary = bars[-1]["T"] + 1
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME, "params": PARAMS}]})
    argv = ["breakout", "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
            "--closed-bar-decisions", "--strategy-refs", refs]
    envelope = _envelope(bars, H, "BTC|1h", cutoff=boundary)
    proc = _run(argv + held, envelope)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    out = json.loads(proc.stdout)
    assert out["closed_bar_decision"]["held"] is False
    assert out["close_strategies"] == [NAME]
    assert out["close_strategy"] == NAME and out["close_fraction"] == 1.0
    assert out["signal"] == close_signal
    same_side = HELD_SHORT if held is HELD_LONG else HELD_LONG
    kept = json.loads(_run(argv + same_side, envelope).stdout)
    assert kept["close_fraction"] == 0.0


def _reordered(bars):
    out = [dict(b) for b in bars]
    out[-30], out[-29] = out[-29], out[-30]
    return out


def _duplicated(bars):
    out = [dict(b) for b in bars]
    out[-30] = dict(out[-31])
    return out


@pytest.mark.parametrize("closed", [True, False])
@pytest.mark.parametrize("defect,untimed_reason,closed_hold", [
    ("negative_volume", "negative_volume", "negative volume"),
    ("reordered", "timestamp_order", "opens before"),
    ("duplicated", "cadence_gap", "share opening time"),
])
def test_invalid_entry_data_holds_the_entry_and_keeps_protection(closed, defect, untimed_reason, closed_hold):
    bars = _entry_bars(1)
    if defect == "negative_volume":
        bars = [dict(b, v=-1.0) if i == len(bars) - 10 else b for i, b in enumerate(bars)]
    elif defect == "reordered":
        bars = _reordered(bars)
    else:
        bars = _duplicated(bars)
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    cutoff = bars[-1]["T"] + 1
    for held, close_signal in ((HELD_LONG, -1), (HELD_SHORT, 1)):
        out = _check(bars, closed=closed, extra=["--strategy-refs", refs] + held, cutoff=cutoff)
        if closed:
            assert out["closed_bar_decision"]["held"] is True
            assert closed_hold in out["closed_bar_decision"]["hold_reason"]
        else:
            assert out["indicator_labels"]["stc_reason"] == untimed_reason
            assert out["indicators"]["stc_valid"] == 0.0
        assert out["open_action"] == "none"
        assert out["close_strategy"] == "tiered_tp_pct" and out["close_fraction"] == 1.0
        assert out["signal"] == close_signal
    flat = _check(bars, closed=closed, extra=["--strategy-refs", refs], cutoff=cutoff)
    assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_missing_timing_holds_the_closed_bar_decision_and_keeps_protection():
    bars = _entry_bars(1)
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    out = _check(bars, closed=True, extra=["--strategy-refs", refs] + HELD_LONG, timing=False,
                 cutoff=bars[-1]["T"] + 1)
    assert out["closed_bar_decision"]["held"] is True
    assert out["indicators"] == {}
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=True, extra=["--strategy-refs", refs], timing=False, cutoff=bars[-1]["T"] + 1)
    assert flat["signal"] == 0 and flat["closed_bar_decision"]["held"] is True


def test_stale_closed_bar_holds_the_entry_and_keeps_protection():
    bars = _entry_bars(1)
    refs = _refs([{"name": "tiered_tp_pct", "params": {}}])
    stale = bars[-1]["T"] + 1 + 3 * H
    out = _check(bars, closed=True, extra=["--strategy-refs", refs] + HELD_LONG, cutoff=stale)
    assert out["closed_bar_decision"]["held"] is False
    assert out["indicator_labels"]["stc_reason"] == "decision_bar_stale"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = _check(bars, closed=True, extra=["--strategy-refs", refs], cutoff=stale)
    assert flat["signal"] == 0 and flat["open_action"] == "none"


@pytest.mark.parametrize("closed", [True, False])
def test_missing_decision_frame_is_an_error_without_a_private_fetch(closed):
    bars = _entry_bars(1)
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs", _refs()]
    if closed:
        argv.append("--closed-bar-decisions")
    market = _market({"ETH|1h": _frame(bars, H)}, cutoff_ms=bars[-1]["T"] + 1, mid=bars[-1]["c"])
    proc = _run(argv, {"v": 2, "market": market})
    assert proc.returncode == 1
    out = _payload(proc)
    assert out["signal"] == 0
    assert "BTC|1h" in out["error"]
