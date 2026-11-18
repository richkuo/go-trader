import os
import sys
from fractions import Fraction

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_ROOT, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

_FUTURES = load_module(
    "_rvi_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_rvi_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_rvi_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "relative_vigor_index"
SMALL = {"period": 3, "zero_line_filter": True}
NEED_SMALL = 3 + 7
TOLERANCE = 1e-12


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def frame(rows, start="2026-01-01", freq="4h"):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


def doji(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px)] * n


def bar(o, c, pad=1.0):
    return (o, max(o, c) + pad, min(o, c) - pad, c)


def reference(df, period, zero_line_filter=True):
    o = [Fraction(float(x)) for x in df["open"]]
    h = [Fraction(float(x)) for x in df["high"]]
    lo = [Fraction(float(x)) for x in df["low"]]
    c = [Fraction(float(x)) for x in df["close"]]
    n = len(o)
    w = (1, 2, 2, 1)
    sn, sd = [None] * n, [None] * n
    for t in range(3, n):
        sn[t] = sum(w[k] * (c[t - k] - o[t - k]) for k in range(4)) / 6
        sd[t] = sum(w[k] * (h[t - k] - lo[t - k]) for k in range(4)) / 6
    rvi = [None] * n
    for t in range(period + 2, n):
        num = sum(sn[t - j] for j in range(period)) / period
        den = sum(sd[t - j] for j in range(period)) / period
        rvi[t] = num / den if den > 0 else None
    sig = [None] * n
    for t in range(period + 5, n):
        vals = [rvi[t - k] for k in range(4)]
        if all(v is not None for v in vals):
            sig[t] = sum(w[k] * vals[k] for k in range(4)) / 6
    events = [0] * n
    edge = [False] * n
    for t in range(period + 6, n):
        if sig[t] is None or sig[t - 1] is None:
            continue
        d0, d1 = rvi[t] - sig[t], rvi[t - 1] - sig[t - 1]
        edge[t] = any(v != 0 and abs(v) <= TOLERANCE for v in (d0, d1, rvi[t]))
        if d1 <= 0 < d0 and (rvi[t] > 0 or not zero_line_filter):
            events[t] = 1
        elif d1 >= 0 > d0 and (rvi[t] < 0 or not zero_line_filter):
            events[t] = -1
    return rvi, sig, events, edge


def market(n=520, seed=1666, step="4h"):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 40 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 4.0))
    open_ = np.concatenate([[close[0]], close[:-1]]) + np.round(rng.randn(n) * 2.0)
    span = np.round(np.abs(rng.randn(n)) * 5.0) + 1.0
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": 1.0})
    df.index = pd.date_range("2025-01-01", periods=n, freq=step)
    return df


def mirror(df, k=4000.0):
    out = df.copy()
    out["open"] = k - df["open"]
    out["close"] = k - df["close"]
    out["high"] = k - df["low"]
    out["low"] = k - df["high"]
    return out


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/relative_vigor_index_1666/REPORT.md")
    assert entry["default_params"] == {"period": 10, "zero_line_filter": True}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


INVALID_PARAMS = [
    {"period": True},
    {"period": 10.5},
    {"period": 1},
    {"period": 101},
    {"period": float("nan")},
    {"period": "10"},
    {"zero_line_filter": 1},
    {"zero_line_filter": "true"},
    {"zero_line_filter": None},
]


def trend(n, step, seed):
    rng = np.random.RandomState(seed)
    base = 1000.0 + step * np.arange(n) + np.round(rng.randn(n) * 3.0)
    open_ = base
    close = base + np.sign(step) * (2.0 + np.abs(np.round(rng.randn(n) * 3.0)))
    high = np.maximum(open_, close) + 1.0
    low = np.minimum(open_, close) - 1.0
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": 1.0})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


COLS = ["signal", "rvi", "rvi_signal", "rvi_diff", "rvi_prev_diff", "rvi_reason", "rvi_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


def _compose(df, position_side, position_ctx=None, params=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        ["time_stop"], position_side, params, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"time_stop": {"max_bars": 5}},
    )
    return evaluation, finalize_decision(evaluation, position_side)


ENTRY = doji(12) + [bar(100.0, 104.0)]


def test_composer_flat_entry_and_open_position_blocks_duplicates_and_reversals():
    _, flat_decision = _compose(frame(ENTRY), "", params=SMALL)
    assert flat_decision["signal"] == 1 and flat_decision["open_action"] == "long"

    held = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, dup = _compose(frame(ENTRY), "long", held, params=SMALL)
    assert dup["open_action"] == "long" and dup["signal"] == 0

    short_ctx = {"side": "short", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, opposite = _compose(frame(ENTRY), "short", short_ctx, params=SMALL)
    assert opposite["close_fraction"] == 0.0 and opposite["signal"] == 0


def test_composer_close_wins_over_entry_and_survives_bad_entry_data():
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(frame(ENTRY), "long", due, params=SMALL)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1

    bad = ENTRY[:-1] + [(100.0, float("nan"), 99.0, 104.0)]
    evaluation, decision = _compose(frame(bad), "long", due, params=SMALL)
    assert evaluation.open_result_df["rvi_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    gapped = frame(ENTRY).drop(frame(ENTRY).index[10])
    evaluation, short_close = _compose(gapped, "short", short_due, params=SMALL)
    assert evaluation.open_result_df["rvi_reason"].iloc[-1] == "cadence_gap"
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = market(360, seed=7)
    full = run(df)
    for i in range(16, len(df)):
        window = df.iloc[max(0, i - 199): i + 1]
        evaluation, decision = _compose(window, "")
        assert evaluation.open_signal == int(full["signal"].iloc[i])
        assert decision["signal"] == int(full["signal"].iloc[i])


def _hl_candles(df):
    return [[int(ts.value // 1_000_000), r.open, r.high, r.low, r.close, r.volume]
            for ts, r in df.iterrows()]


def test_production_hyperliquid_paths_admit_explicit_paper_and_refuse_unacknowledged_live():
    df = _CHECK_HL._make_dataframe(_hl_candles(market(200)))
    shared = _CHECK_HL.build_shared_signal_state("BTC", "4h", df=df)
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "solo", "strategy": NAME})
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "open", "strategy": "breakout", "open_strategy": NAME,
                     "close_strategies": "time_stop"})
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "close", "strategy": "breakout", "open_strategy": "breakout",
                     "close_strategies": NAME})
    deps = _CHECK_HL._signal_check_deps()
    peer = _CHECK_HL.evaluate_signal_slot(shared, {"id": "peer", "strategy": "breakout"}, deps=deps)
    assert peer["strategy"] == "breakout"
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "batched", "strategy": NAME}, deps=deps)
    paper = _CHECK_HL.evaluate_signal_slot(shared, {"id": "paper", "strategy": NAME, "mode_args": ["--mode=paper"]})
    acked = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "acked", "strategy": NAME, "mode_args": ["--mode=live"], "allow_no_edge": True})
    assert paper["strategy"] == acked["strategy"] == NAME
    assert (paper["signal"], paper["price"]) == (acked["signal"], acked["price"])
    fallback = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "fallback", "strategy": "breakout", "open_strategy": "breakout",
                 "close_strategies": NAME, "mode_args": ["--mode", "paper"]})
    assert fallback["close_strategies"] == [NAME]


