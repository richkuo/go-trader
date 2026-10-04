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
    "_cct_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_cct_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_cct_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "commodity_channel_trend"
SMALL = {"lookback": 5, "threshold": 1e9, "trend_period": 3}


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def frame(rows, start="2026-01-01", freq="4h"):
    df = pd.DataFrame(rows, columns=["high", "low", "close"], dtype=float)
    df.insert(0, "open", df["close"])
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


def base(n=6):
    return [(101.0, 99.0, 100.0) if i % 2 == 0 else (102.0, 100.0, 101.0) for i in range(n)]


UP_ABOVE = base() + [(102.0, 100.5, 101.5), (112.0, 102.0, 110.0)]
UP_EQUAL = base() + [(101.0, 99.5, 100.0), (102.5, 100.5, 102.0), (116.0, 100.5, 101.0)]
UP_BELOW = base() + [(103.0, 101.5, 102.5), (104.0, 102.5, 103.5), (118.0, 100.5, 101.0)]
DOWN_BELOW = base() + [(100.5, 99.0, 99.5), (99.0, 89.0, 91.0)]
DOWN_EQUAL = base() + [(102.5, 100.5, 102.0), (101.0, 99.5, 100.0), (101.5, 86.0, 101.0)]


def indices(rows, params=SMALL):
    out = run(frame(rows), params)
    return float(out["cct_index"].iloc[-2]), float(out["cct_index"].iloc[-1]), out.iloc[-1]


def last(rows, params):
    return run(frame(rows), params).iloc[-1]


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/commodity_channel_trend_1656/REPORT.md")
    assert entry["default_params"] == {"lookback": 20, "threshold": 100.0, "trend_period": 50}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


def test_long_cross_truth_table_rows_1_4_5_6():
    prev, cur, row = indices(UP_ABOVE)
    assert 0 < prev < cur
    assert row["close"] > row["cct_trend_sma"]
    mid = (prev + cur) / 2.0
    hit = last(UP_ABOVE, {**SMALL, "threshold": mid})
    assert int(hit["signal"]) == 1 and hit["cct_reason"] == "entry_long"
    at_cur = last(UP_ABOVE, {**SMALL, "threshold": cur})
    assert at_cur["cct_index"] == at_cur["cct_threshold"]
    assert int(at_cur["signal"]) == 0 and at_cur["cct_reason"] == "no_cross"
    at_prev = last(UP_ABOVE, {**SMALL, "threshold": prev})
    assert at_prev["cct_prev_index"] == at_prev["cct_threshold"]
    assert int(at_prev["signal"]) == 1 and at_prev["cct_reason"] == "entry_long"
    below_both = last(UP_ABOVE, {**SMALL, "threshold": prev / 2.0})
    assert int(below_both["signal"]) == 0 and below_both["cct_reason"] == "no_cross"


@pytest.mark.parametrize("rows,relation", [(UP_EQUAL, "equal"), (UP_BELOW, "below")])
def test_long_cross_blocked_by_trend_rows_2_3(rows, relation):
    prev, cur, row = indices(rows)
    assert 0 < cur and prev < cur
    if relation == "equal":
        assert row["close"] == row["cct_trend_sma"]
    else:
        assert row["close"] < row["cct_trend_sma"]
    out = last(rows, {**SMALL, "threshold": (max(prev, 0.0) + cur) / 2.0})
    assert int(out["signal"]) == 0
    assert out["cct_reason"] == "trend_blocked"


def test_short_cross_truth_table_rows_7_8_9():
    prev, cur, row = indices(DOWN_BELOW)
    assert cur < prev < 0
    assert row["close"] < row["cct_trend_sma"]
    hit = last(DOWN_BELOW, {**SMALL, "threshold": -(prev + cur) / 2.0})
    assert int(hit["signal"]) == -1 and hit["cct_reason"] == "entry_short"
    at_cur = last(DOWN_BELOW, {**SMALL, "threshold": -cur})
    assert at_cur["cct_index"] == -at_cur["cct_threshold"]
    assert int(at_cur["signal"]) == 0 and at_cur["cct_reason"] == "no_cross"
    at_prev = last(DOWN_BELOW, {**SMALL, "threshold": -prev})
    assert int(at_prev["signal"]) == -1

    eprev, ecur, erow = indices(DOWN_EQUAL)
    assert ecur < 0 and ecur < eprev
    assert erow["close"] == erow["cct_trend_sma"]
    blocked = last(DOWN_EQUAL, {**SMALL, "threshold": -(min(eprev, 0.0) + ecur) / 2.0})
    assert int(blocked["signal"]) == 0 and blocked["cct_reason"] == "trend_blocked"


def test_zero_mean_deviation_rows_10_11():
    flat = [(101.0, 99.0, 100.0)] * 8
    row = last(flat, {**SMALL, "threshold": 1.0})
    assert int(row["signal"]) == 0
    assert row["cct_reason"] == "zero_mean_deviation"
    assert row["cct_mean_dev"] == 0.0 and np.isnan(row["cct_index"])
    after = last(flat + [(110.0, 100.0, 105.0)], {**SMALL, "threshold": 1.0})
    assert after["cct_index"] > 1.0
    assert int(after["signal"]) == 0
    assert after["cct_reason"] == "zero_mean_deviation"
    assert np.isnan(after["cct_prev_index"])


def test_insufficient_history_row_12_and_missing_columns():
    p = {"lookback": 3, "threshold": 1.0, "trend_period": 5}
    short = run(frame(base(4)), p)
    assert (short["cct_reason"] == "insufficient_history").all()
    assert (short["signal"] == 0).all()
    exact = run(frame(base(5)), p)
    assert bool(exact["cct_valid"].iloc[-1])
    missing = run(frame(base(8)).drop(columns=["low"]), SMALL)
    assert (missing["signal"] == 0).all()
    assert missing["cct_reason"].iloc[-1] == "missing_columns:low"


@pytest.mark.parametrize("mutate,reason", [
    (lambda r: (r[1], r[0], r[2]), "inconsistent_hlc"),
    (lambda r: (r[0], r[1], r[0] + 5.0), "inconsistent_hlc"),
    (lambda r: (float("inf"), r[1], r[2]), "nonfinite_input"),
    (lambda r: (r[0], r[1], float("nan")), "nonfinite_input"),
])
def test_malformed_bar_row_13_yields_reasoned_hold(mutate, reason):
    rows = list(UP_ABOVE)
    rows[-1] = mutate(rows[-1])
    row = last(rows, {**SMALL, "threshold": 1.0})
    assert int(row["signal"]) == 0
    assert not bool(row["cct_valid"])
    assert row["cct_reason"] == reason
    earlier = list(UP_ABOVE)
    earlier[-2] = mutate(earlier[-2])
    row2 = last(earlier, {**SMALL, "threshold": 1.0})
    assert int(row2["signal"]) == 0
    assert row2["cct_reason"] == reason


def test_timestamp_order_defect_detected():
    df = frame(UP_ABOVE)
    idx = list(df.index)
    idx[-1] = idx[-2]
    df.index = pd.DatetimeIndex(idx)
    row = run(df, {**SMALL, "threshold": 1.0}).iloc[-1]
    assert row["cct_reason"] == "timestamp_order"
    assert int(row["signal"]) == 0
    with_col = frame(UP_ABOVE).reset_index(drop=True)
    with_col["timestamp"] = [1_000 * i for i in range(len(with_col))]
    with_col.loc[len(with_col) - 1, "timestamp"] = 3_000
    assert run(with_col, SMALL).iloc[-1]["cct_reason"] == "timestamp_order"


@pytest.mark.parametrize("params", [
    {"lookback": True},
    {"lookback": 20.5},
    {"lookback": 1},
    {"lookback": 101},
    {"trend_period": 1},
    {"trend_period": 151},
    {"trend_period": False},
    {"threshold": 0.0},
    {"threshold": -5.0},
    {"threshold": float("nan")},
    {"threshold": float("inf")},
    {"threshold": True},
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(frame(base(200)), params)


def test_integral_float_periods_accepted():
    out = run(frame(base(80)), {"lookback": 20.0, "trend_period": 50.0})
    assert out["cct_reason"].iloc[-1] in ("no_cross", "trend_blocked")


def _trend(n, step, seed):
    rng = np.random.RandomState(seed)
    close = 100.0 + step * np.arange(n) + rng.randn(n) * 1.5
    span = 0.3 + np.abs(rng.randn(n)) * 0.6
    df = pd.DataFrame({"open": close, "high": close + span, "low": close - span,
                       "close": close, "volume": 1.0})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


def test_rising_series_only_longs_falling_only_shorts_constant_none():
    up = run(_trend(400, 0.5, 1))
    assert (up["signal"] >= 0).all() and (up["signal"] == 1).any()
    down = run(_trend(400, -0.5, 2))
    assert (down["signal"] <= 0).all() and (down["signal"] == -1).any()
    const = run(frame([(101.0, 99.0, 100.0)] * 120))
    assert (const["signal"] == 0).all()


def _market(n=520, seed=1656):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 60000 + 3000 * np.sin(t / 30.0) + np.cumsum(rng.randn(n) * 250)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 300 + 20
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close,
                       "volume": 1.0})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


COLS = ["signal", "cct_typical", "cct_mean", "cct_mean_dev", "cct_index", "cct_prev_index",
        "cct_trend_sma", "cct_reason", "cct_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [
    None,
    {"lookback": 100, "threshold": 50.0, "trend_period": 150},
    {"lookback": 14, "threshold": 150.0, "trend_period": 20},
])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = _market()
    full = run(df, params)
    p = {**_FUTURES.STRATEGY_REGISTRY[NAME]["default_params"], **(params or {})}
    need = max(p["lookback"] + 1, p["trend_period"])
    assert (full["signal"] != 0).sum() > 0
    for i in range(len(df)):
        prefix = run(df.iloc[: i + 1], params).iloc[-1]
        _same(full.iloc[i], prefix)
        tail = run(df.iloc[max(0, i - 199): i + 1], params).iloc[-1]
        if i + 1 >= need:
            _same(full.iloc[i], tail)
    exact = run(df.iloc[len(df) - need:], params).iloc[-1]
    _same(full.iloc[-1], exact)
    too_short = run(df.iloc[len(df) - need + 1:], params).iloc[-1]
    assert too_short["cct_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_close_exactly_at_trend_average_matches_on_bounded_window():
    rows = base(220) + UP_EQUAL[6:]
    df = frame(rows)
    p = {"lookback": 5, "threshold": 150.0, "trend_period": 3}
    full = run(df, p).iloc[-1]
    tail = run(df.iloc[-200:], p).iloc[-1]
    assert full["close"] == full["cct_trend_sma"]
    _same(full, tail)
    assert full["cct_reason"] == "trend_blocked"


def test_every_entry_is_reproducible_from_recorded_columns():
    df = _market()
    out = run(df)
    entries = out[out["signal"] != 0]
    assert len(entries) > 0
    for ts, row in entries.iterrows():
        i = df.index.get_loc(ts)
        tp = ((df["high"] + df["low"] + df["close"]) / 3.0).to_numpy()
        for j, col in ((i, "cct_index"), (i - 1, "cct_prev_index")):
            win = tp[j - 19: j + 1]
            mean = win.mean()
            dev = np.abs(win - mean).mean()
            assert row[col] == pytest.approx((tp[j] - mean) / (0.015 * dev), rel=1e-9)
        assert row["cct_trend_sma"] == pytest.approx(df["close"].iloc[i - 49: i + 1].mean(), rel=1e-12)
        assert row["cct_eval_ts_ms"] == ts.value // 1_000_000
        t = row["cct_threshold"]
        if row["signal"] == 1:
            assert row["cct_prev_index"] <= t < row["cct_index"]
            assert row["close"] > row["cct_trend_sma"]
            assert row["cct_reason"] == "entry_long"
        else:
            assert row["cct_prev_index"] >= -t > row["cct_index"]
            assert row["close"] < row["cct_trend_sma"]
            assert row["cct_reason"] == "entry_short"
    assert df.equals(_market())


def _compose(df, position_side, position_ctx=None, close_strategies=("time_stop",), params=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        list(close_strategies), position_side, params, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"time_stop": {"max_bars": 5}},
    )
    return evaluation, finalize_decision(evaluation, position_side)


def _entry_params():
    prev, cur, _ = indices(UP_ABOVE)
    return {**SMALL, "threshold": (prev + cur) / 2.0}


def test_composer_flat_entry_and_open_position_blocks_duplicates_and_reversals():
    params = _entry_params()
    _, flat_decision = _compose(frame(UP_ABOVE), "", params=params)
    assert flat_decision["signal"] == 1 and flat_decision["open_action"] == "long"

    held = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, dup = _compose(frame(UP_ABOVE), "long", held, params=params)
    assert dup["open_action"] == "long" and dup["signal"] == 0

    short_ctx = {"side": "short", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, opposite = _compose(frame(UP_ABOVE), "short", short_ctx, params=params)
    assert opposite["close_fraction"] == 0.0 and opposite["signal"] == 0


def test_composer_close_wins_and_survives_bad_entry_data():
    params = _entry_params()
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(frame(UP_ABOVE), "long", due, params=params)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1

    bad = UP_ABOVE[:-1] + [(110.0, 100.5, float("nan"))]
    evaluation, decision = _compose(frame(bad), "long", due, params=params)
    assert evaluation.open_result_df["cct_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(frame(base(8)).drop(columns=["high"]), "short", short_due, params=SMALL)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = _market(360, seed=7)
    full = run(df)
    for i in range(49, len(df)):
        window = df.iloc[max(0, i - 199): i + 1]
        evaluation, decision = _compose(window, "", close_strategies=("time_stop",))
        assert evaluation.open_signal == int(full["signal"].iloc[i])
        assert decision["signal"] == int(full["signal"].iloc[i])


def _hl_candles(df):
    return [[int(ts.value // 1_000_000), r.open, r.high, r.low, r.close, r.volume]
            for ts, r in df.iterrows()]


def test_production_hyperliquid_paths_admit_explicit_paper_and_refuse_unacknowledged_live():
    df = _CHECK_HL._make_dataframe(_hl_candles(_market(200)))
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


