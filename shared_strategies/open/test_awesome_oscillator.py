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
    "_ao_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_ao_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_ao_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "awesome_oscillator"
SMALL = {"fast_period": 1, "slow_period": 2}
NEED_SMALL = 2 + 8
HORIZON = 8


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def frame(mids, start="2026-01-01", freq="4h"):
    mids = np.asarray(mids, dtype=float)
    df = pd.DataFrame({"open": mids, "high": mids + 1.0, "low": mids - 1.0, "close": mids,
                       "volume": np.ones(len(mids))})
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


def ao_frame(ao_values, base=100.0):
    mids = [base]
    for a in ao_values:
        mids.append(mids[-1] + 2.0 * a)
    return frame(mids)


def test_registered_research_only_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert entry["backtest_only"] is True
    assert entry["default_params"] == {"fast_period": 5, "slow_period": 34}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


def test_oscillator_equals_half_the_midpoint_step_for_one_and_two_bar_means():
    out = run(ao_frame([0.0] * 9 + [-1.0, 3.0]), SMALL)
    assert out["ao"].iloc[-1] == 3.0
    assert out["ao"].iloc[-2] == -1.0


@pytest.mark.parametrize("tail,signal,reason,lag", [
    ([-1.0, 2.0], 1, "entry_long", 1.0),
    ([-1.0] + [0.0] * 7 + [2.0], 1, "entry_long", 8.0),
    ([1.0, 2.0], 0, "no_cross", 1.0),
    ([-1.0] + [0.0] * 8 + [2.0], 0, "zero_run_exceeded", np.nan),
    ([-1.0, 0.0], 0, "zero_oscillator", 1.0),
    ([1.0, -2.0], -1, "entry_short", 1.0),
    ([1.0] + [0.0] * 7 + [-2.0], -1, "entry_short", 8.0),
    ([-1.0, -2.0], 0, "no_cross", 1.0),
])
def test_truth_table_cases(tail, signal, reason, lag):
    out = run(ao_frame([0.5] * 12 + tail), SMALL)
    row = out.iloc[-1]
    assert int(row["signal"]) == signal
    assert row["ao_reason"] == reason
    assert bool(row["ao_valid"])
    if np.isnan(lag):
        assert np.isnan(row["ao_prev_lag"])
    else:
        assert row["ao_prev_lag"] == lag
        assert row["ao_prev_nonzero"] == tail[-1 - int(lag)]
    if signal:
        assert np.sign(row["ao"]) == signal == -np.sign(row["ao_prev_nonzero"])


def test_long_zero_run_never_crosses_in_full_series_or_bounded_window():
    ao = [-1.0] * 20 + [0.0] * 250 + [2.0]
    df = ao_frame(ao)
    full = run(df)
    assert (full["ao"].iloc[-201:-1] == 0.0).all()
    assert int(full["signal"].iloc[-1]) == 0
    assert full["ao_reason"].iloc[-1] == "zero_run_exceeded"
    tail = run(df.iloc[-200:]).iloc[-1]
    assert int(tail["signal"]) == 0
    assert tail["ao_reason"] == "zero_run_exceeded"
    assert (full["signal"] == 0).all()


def test_invalid_value_inside_predecessor_search_resets_it():
    ao = [0.5] * 12 + [-1.0, 0.0, 0.0, 0.0, 2.0]
    clean = run(ao_frame(ao), SMALL)
    assert int(clean["signal"].iloc[-1]) == 1
    assert clean["ao_prev_lag"].iloc[-1] == 4.0
    bad = ao_frame(ao)
    bad.iloc[-3, bad.columns.get_loc("high")] = np.nan
    out = run(bad, SMALL)
    assert not bool(out["ao_valid"].iloc[-1])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["ao_reason"].iloc[-1] == "nonfinite_input"
    recover = run(ao_frame([0.5] * 12 + [-1.0] + [-0.5] * 12 + [2.0]), SMALL)
    bad2 = ao_frame([0.5] * 12 + [-1.0] + [-0.5] * 12 + [2.0])
    bad2.iloc[5, bad2.columns.get_loc("low")] = bad2["high"].iloc[5] + 5.0
    out2 = run(bad2, SMALL)
    assert out2["ao_reason"].iloc[6] == "inconsistent_hl"
    assert int(out2["signal"].iloc[-1]) == int(recover["signal"].iloc[-1]) == 1


@pytest.mark.parametrize("col,value,reason", [
    ("high", np.inf, "nonfinite_input"),
    ("low", np.nan, "nonfinite_input"),
    ("low", 1e9, "inconsistent_hl"),
])
def test_malformed_bar_yields_reasoned_hold(col, value, reason):
    df = ao_frame([0.5] * 12 + [-1.0, 2.0])
    df.iloc[-1, df.columns.get_loc(col)] = value
    row = run(df, SMALL).iloc[-1]
    assert int(row["signal"]) == 0
    assert not bool(row["ao_valid"])
    assert row["ao_reason"] == reason


def test_insufficient_history_and_missing_columns():
    short = run(ao_frame([-1.0] * 5 + [2.0]), SMALL)
    assert (short["ao_reason"] == "insufficient_history").all()
    assert (short["signal"] == 0).all()
    exact = run(ao_frame([0.5] * (NEED_SMALL - 3) + [-1.0, 2.0]), SMALL)
    assert len(exact) == NEED_SMALL
    assert int(exact["signal"].iloc[-1]) == 1
    one_short = run(ao_frame([0.5] * (NEED_SMALL - 4) + [-1.0, 2.0]), SMALL)
    assert one_short["ao_reason"].iloc[-1] == "insufficient_history"
    assert int(one_short["signal"].iloc[-1]) == 0
    missing = run(frame([100.0] * 20).drop(columns=["low"]), SMALL)
    assert (missing["signal"] == 0).all()
    assert missing["ao_reason"].iloc[-1] == "missing_columns:low"


def test_timestamp_order_defect_detected():
    df = ao_frame([0.5] * 12 + [-1.0, 2.0])
    idx = list(df.index)
    idx[-1] = idx[-2]
    df.index = pd.DatetimeIndex(idx)
    row = run(df, SMALL).iloc[-1]
    assert row["ao_reason"] == "timestamp_order"
    assert int(row["signal"]) == 0
    with_col = ao_frame([0.5] * 12 + [-1.0, 2.0]).reset_index(drop=True)
    with_col["timestamp"] = [1_000 * i for i in range(len(with_col))]
    with_col.loc[len(with_col) - 1, "timestamp"] = 3_000
    assert run(with_col, SMALL).iloc[-1]["ao_reason"] == "timestamp_order"


def test_constant_series_is_exact_zero_and_never_emits():
    out = run(frame([100.0] * 120))
    valid = out[out["ao_valid"]]
    assert len(valid) > 0
    assert (valid["ao"] == 0.0).all()
    assert (valid["ao_reason"] == "zero_oscillator").all()
    assert (out["signal"] == 0).all()


@pytest.mark.parametrize("params", [
    {"fast_period": True},
    {"fast_period": 5.5},
    {"fast_period": 0},
    {"slow_period": 101},
    {"fast_period": 34, "slow_period": 34},
    {"fast_period": 40, "slow_period": 34},
    {"slow_period": float("nan")},
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(frame([100.0] * 60), params)


def test_integral_float_period_accepted():
    out = run(frame([100.0] * 60), {"fast_period": 5.0, "slow_period": 34.0})
    assert out["ao_reason"].iloc[-1] == "zero_oscillator"


def _trend(n, step, seed):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    mid = 100.0 + step * t + 6.0 * np.sin(t / 9.0) + rng.randn(n) * 0.3
    span = 0.5 + np.abs(rng.randn(n)) * 0.2
    df = pd.DataFrame({"open": mid, "high": mid + span, "low": mid - span, "close": mid,
                       "volume": np.ones(n)})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


def test_rising_and_falling_series_alternate_entries_by_sign():
    for step in (0.4, -0.4):
        out = run(_trend(400, step, 3))
        entries = out[out["signal"] != 0]
        assert len(entries) > 0
        for _, row in entries.iterrows():
            assert np.sign(row["ao"]) == row["signal"]
            assert np.sign(row["ao_prev_nonzero"]) == -row["signal"]


def _market(n=560, seed=1660):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 8 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.8 + 0.1
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close,
                       "volume": np.ones(n)})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


COLS = ["signal", "ao", "ao_fast_mean", "ao_slow_mean", "ao_prev_nonzero", "ao_prev_lag",
        "ao_reason", "ao_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [None, {"fast_period": 13, "slow_period": 100}])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = _market()
    full = run(df, params)
    p = {**_FUTURES.STRATEGY_REGISTRY[NAME]["default_params"], **(params or {})}
    need = p["slow_period"] + HORIZON
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
    assert too_short["ao_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_every_entry_is_reproducible_from_recorded_columns():
    df = _market()
    out = run(df)
    entries = out[out["signal"] != 0]
    assert len(entries) > 0
    mid = ((df["high"] + df["low"]) / 2.0).to_numpy()
    for ts, row in entries.iterrows():
        i = df.index.get_loc(ts)
        fast = float(np.mean(mid[i - 4: i + 1]))
        slow = float(np.mean(mid[i - 33: i + 1]))
        assert row["ao_fast_mean"] == pytest.approx(fast, rel=1e-12)
        assert row["ao_slow_mean"] == pytest.approx(slow, rel=1e-12)
        assert row["ao"] == row["ao_fast_mean"] - row["ao_slow_mean"]
        lag = int(row["ao_prev_lag"])
        assert out["ao"].iloc[i - lag] == row["ao_prev_nonzero"]
        assert (out["ao"].iloc[i - lag + 1: i] == 0.0).all()
        assert row["ao_eval_ts_ms"] == ts.value // 1_000_000
        assert np.sign(row["ao"]) == row["signal"] == -np.sign(row["ao_prev_nonzero"])
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


ENTRY = [0.5] * 12 + [-1.0, 2.0]


def test_composer_flat_entry_and_open_position_blocks_duplicates_and_reversals():
    _, flat_decision = _compose(ao_frame(ENTRY), "", params=SMALL)
    assert flat_decision["signal"] == 1 and flat_decision["open_action"] == "long"

    held = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, dup = _compose(ao_frame(ENTRY), "long", held, params=SMALL)
    assert dup["open_action"] == "long" and dup["signal"] == 0

    short_ctx = {"side": "short", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, opposite = _compose(ao_frame(ENTRY), "short", short_ctx, params=SMALL)
    assert opposite["close_fraction"] == 0.0 and opposite["signal"] == 0


def test_composer_close_wins_over_simultaneous_entry_and_survives_bad_entry_data():
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(ao_frame(ENTRY), "long", due, params=SMALL)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1

    bad = ao_frame(ENTRY)
    bad.iloc[-1, bad.columns.get_loc("high")] = np.nan
    evaluation, decision = _compose(bad, "long", due, params=SMALL)
    assert evaluation.open_result_df["ao_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(frame([100.0] * 20).drop(columns=["low"]), "short", short_due, params=SMALL)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = _market(360, seed=7)
    full = run(df)
    need = 34 + HORIZON
    for i in range(need - 1, len(df)):
        window = df.iloc[max(0, i - 199): i + 1]
        evaluation, decision = _compose(window, "", close_strategies=("time_stop",))
        assert evaluation.open_signal == int(full["signal"].iloc[i])
        assert decision["signal"] == int(full["signal"].iloc[i])


def _hl_candles(df):
    return [[int(ts.value // 1_000_000), r.open, r.high, r.low, r.close, r.volume]
            for ts, r in df.iterrows()]


def test_production_hyperliquid_paths_reject_research_entry_and_close_fallback():
    df = _CHECK_HL._make_dataframe(_hl_candles(_market(200)))
    shared = _CHECK_HL.build_shared_signal_state("BTC", "4h", df=df)
    with pytest.raises(ValueError, match="backtest_only"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "solo", "strategy": NAME})
    with pytest.raises(ValueError, match="backtest_only"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "open", "strategy": "sma_crossover", "open_strategy": NAME,
                     "close_strategies": "time_stop"})
    with pytest.raises(ValueError, match="backtest_only"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "close", "strategy": "sma_crossover", "open_strategy": "sma_crossover",
                     "close_strategies": NAME})
    deps = _CHECK_HL._signal_check_deps()
    peer = _CHECK_HL.evaluate_signal_slot(shared, {"id": "peer", "strategy": "sma_crossover"}, deps=deps)
    assert peer["strategy"] == "sma_crossover"
    with pytest.raises(ValueError, match="backtest_only"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "batched", "strategy": NAME}, deps=deps)
