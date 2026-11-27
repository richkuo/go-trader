import os
import sys

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_ROOT, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

_FUTURES = load_module(
    "_vortex_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "vortex_trend"
SMALL = {"period": 2, "min_separation": 0.0}


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def frame(rows, start="2026-01-01", freq="4h"):
    df = pd.DataFrame(rows, columns=["high", "low", "close"], dtype=float)
    df.insert(0, "open", df["close"])
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


FLAT = (101.0, 99.0, 100.0)
UP = (103.0, 101.0, 102.0)
DOWN = (99.0, 97.0, 98.0)


def flat(n):
    return [FLAT] * n


def reference(df, period, sep):
    h, l, c = df["high"], df["low"], df["close"]
    vmp = (h - l.shift(1)).abs()
    vmm = (l - h.shift(1)).abs()
    tr = pd.concat([h - l, (h - c.shift(1)).abs(), (l - c.shift(1)).abs()], axis=1).max(axis=1)
    tr[c.shift(1).isna()] = np.nan
    s_tr = tr.rolling(period, min_periods=period).sum()
    vip = vmp.rolling(period, min_periods=period).sum() / s_tr
    vim = vmm.rolling(period, min_periods=period).sum() / s_tr
    d = vip - vim
    prev = d.shift(1)
    sig = np.where((d > sep) & (prev <= sep), 1, np.where((d < -sep) & (prev >= -sep), -1, 0))
    return vip, vim, d, pd.Series(sig, index=df.index)


MALFORMED_BAR_CASES = [
    ("swapped_high_low", lambda r: (r[1], r[0], r[2]), "inconsistent_hlc"),
    ("close_above_high", lambda r: (r[0], r[1], r[0] + 5.0), "inconsistent_hlc"),
    ("infinite_high", lambda r: (float("inf"), r[1], r[2]), "nonfinite_input"),
    ("nan_low", lambda r: (r[0], float("nan"), r[2]), "nonfinite_input"),
]


def test_timestamp_order_defect_detected():
    df = frame(flat(8))
    idx = list(df.index)
    idx[-1] = idx[-2]
    df.index = pd.DatetimeIndex(idx)
    row = run(df, SMALL).iloc[-1]
    assert row["vortex_reason"] == "timestamp_order"
    assert int(row["signal"]) == 0
    with_col = frame(flat(8)).reset_index(drop=True)
    with_col["timestamp"] = [1_000 * i for i in range(8)]
    with_col.loc[7, "timestamp"] = 3_000
    assert run(with_col, SMALL).iloc[-1]["vortex_reason"] == "timestamp_order"


INVALID_PARAMS = [
    {"period": True},
    {"period": 14.5},
    {"period": 1},
    {"period": 101},
    {"min_separation": 1.0},
    {"min_separation": -0.01},
    {"min_separation": float("nan")},
    {"min_separation": False},
]


COLS = ["signal", "vortex_vi_plus", "vortex_vi_minus", "vortex_diff", "vortex_prev_diff",
        "vortex_tr_sum", "vortex_reason", "vortex_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


def _compose(df, position_side, position_ctx=None, close_strategies=("time_stop",), params=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        list(close_strategies), position_side, params, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"time_stop": {"max_bars": 5}},
    )
    return evaluation, finalize_decision(evaluation, position_side)


ENTRY = flat(3) + [UP]


def test_composer_close_wins_over_simultaneous_entry_and_survives_bad_entry_data():
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(frame(ENTRY), "long", due, params=SMALL)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1

    bad = ENTRY[:-1] + [(103.0, 101.0, float("nan"))]
    evaluation, decision = _compose(frame(bad), "long", due, params=SMALL)
    assert evaluation.open_result_df["vortex_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(frame(flat(8)).drop(columns=["high"]), "short", short_due, params=SMALL)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1
