import os
import sys

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
    "_uo_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "ultimate_oscillator"
DEFAULTS = {"short_window": 7, "mid_window": 14, "long_window": 28,
            "bullish_threshold": 70.0, "bearish_threshold": 30.0, "breakout_lookback": 20}
NEED = 29
TIERS = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
COLS = ["signal", "uo", "uo_avg_short", "uo_avg_mid", "uo_avg_long", "uo_bp_sum_long",
        "uo_tr_sum_long", "donchian_upper", "donchian_lower", "uo_raw_event", "uo_valid",
        "uo_reason", "uo_reason_code"]


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def market(n=500, seed=1664, freq="1h", start="2025-01-01", base=200.0):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    drift = np.where((t // 60) % 2 == 0, 0.45, -0.45)
    close = base + np.cumsum(drift + rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.5 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": np.ones(n)})
    df.index = pd.date_range(start, periods=n, freq=freq)
    return df


def reference(df, i, p):
    h, l, c = (df[k].to_numpy(dtype=float) for k in ("high", "low", "close"))

    def avg(w):
        bp = tr = 0.0
        for k in range(i - w + 1, i + 1):
            floor = min(l[k], c[k - 1])
            bp += c[k] - floor
            tr += max(h[k], c[k - 1]) - floor
        return bp / tr

    uo = 100.0 * (4 * avg(p["short_window"]) + 2 * avg(p["mid_window"]) + avg(p["long_window"])) / 7
    L = p["breakout_lookback"]
    upper, lower = max(h[i - L:i]), min(l[i - L:i])
    prev_upper, prev_lower = max(h[i - 1 - L:i - 1]), min(l[i - 1 - L:i - 1])
    event = 1 if (c[i] > upper and c[i - 1] <= prev_upper) else (
        -1 if (c[i] < lower and c[i - 1] >= prev_lower) else 0)
    return uo, upper, lower, event


def same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


def test_oscillator_and_events_match_an_independent_calculation():
    df = market()
    out = run(df)
    valid = np.flatnonzero(out["uo_valid"].to_numpy())
    assert valid[0] == NEED - 1 and len(valid) == len(df) - NEED + 1
    for i in valid:
        uo, upper, lower, event = reference(df, i, DEFAULTS)
        row = out.iloc[i]
        assert row["uo"] == pytest.approx(uo, rel=1e-12, abs=1e-12)
        assert 0.0 <= row["uo"] <= 100.0
        assert row["donchian_upper"] == upper and row["donchian_lower"] == lower
        assert row["uo_raw_event"] == event
        expected = 1 if event == 1 and uo > 70.0 else (-1 if event == -1 and uo < 30.0 else 0)
        assert row["signal"] == expected
    donchian = _FUTURES.apply_strategy("donchian_breakout", df, {"entry_period": 20, "exit_period": 10})
    assert (donchian["signal"].to_numpy()[valid] == out["uo_raw_event"].to_numpy()[valid]).all()
    assert (out["signal"] == 1).sum() > 0 and (out["signal"] == -1).sum() > 0


def test_appending_a_smaller_future_step_never_changes_earlier_decisions():
    df = market(260, freq="4h")
    base = run(df)
    extra = market(40, seed=99, freq="1h", start=str(df.index[-1] + pd.Timedelta(hours=1)))
    full = run(pd.concat([df, extra]))
    for i in range(len(df)):
        same(base.iloc[i], full.iloc[i])
    assert (full["uo_reason"].iloc[len(df):len(df) + NEED - 2] == "cadence_gap").all()
    assert full["uo_valid"].iloc[len(df) + NEED - 2]


def _compose(df, position_side, position_ctx=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        ["tiered_tp_pct"], position_side, None, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"tiered_tp_pct": TIERS},
    )
    return evaluation, finalize_decision(evaluation, position_side)


def _entry_slice():
    df = market()
    out = run(df)
    i = int(np.flatnonzero(out["signal"].to_numpy() == 1)[0])
    return df.iloc[: i + 1], float(df["close"].iloc[i])


def test_composer_entry_suppression_and_close_priority():
    window, mark = _entry_slice()
    _, flat = _compose(window, "")
    assert flat["signal"] == 1 and flat["open_action"] == "long"
    held = {"side": "long", "bars_held": 3, "avg_cost": mark, "current_quantity": 1.0}
    _, dup = _compose(window, "long", held)
    assert dup["signal"] == 0 and dup["close_fraction"] == 0.0
    short_ctx = {"side": "short", "bars_held": 3, "avg_cost": mark, "current_quantity": 1.0}
    _, opposite = _compose(window, "short", short_ctx)
    assert opposite["signal"] == 0 and opposite["close_fraction"] == 0.0

    win_ctx = {"side": "long", "bars_held": 3, "avg_cost": mark / 1.04, "current_quantity": 1.0}
    _, both = _compose(window, "long", win_ctx)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1
    short_win = {"side": "short", "bars_held": 3, "avg_cost": mark * 1.04, "current_quantity": 1.0}
    _, short_close = _compose(window, "short", short_win)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


