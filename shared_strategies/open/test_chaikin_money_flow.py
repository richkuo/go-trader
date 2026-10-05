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
    "_cmf_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_cmf_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_cmf_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "chaikin_money_flow_breakout"
SMALL = {"flow_window": 3, "breakout_window": 3, "flow_threshold": 0.2}


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def frame(rows, start="2026-01-01", freq="4h"):
    df = pd.DataFrame(rows, columns=["high", "low", "close", "volume"], dtype=float)
    df.insert(0, "open", df["close"])
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


def flat(n):
    return [(101.0, 99.0, 100.0, 1.0)] * n


def last(df, params=SMALL):
    out = run(df, params)
    return out.iloc[-1]


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/chaikin_money_flow_1649/REPORT.md")
    assert entry["default_params"] == {"flow_window": 20, "breakout_window": 20, "flow_threshold": 0.05}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


@pytest.mark.parametrize("bar,expected_signal,expected_reason", [
    ((110.0, 100.0, 110.0, 8.0), 1, "entry_long"),
    ((110.0, 100.0, 110.0, 0.5), 0, "flow_below_threshold"),
    ((110.0, 100.0, 110.0, 0.2), 0, "flow_below_threshold"),
    ((101.0, 99.0, 101.0, 8.0), 0, "no_breakout"),
    ((100.0, 90.0, 90.0, 8.0), -1, "entry_short"),
    ((100.0, 90.0, 90.0, 0.5), 0, "flow_below_threshold"),
    ((101.0, 99.0, 99.0, 8.0), 0, "no_breakout"),
])
def test_truth_table_single_bar_cases(bar, expected_signal, expected_reason):
    row = last(frame(flat(6) + [bar]))
    assert int(row["signal"]) == expected_signal
    assert row["cmf_reason"] == expected_reason
    assert bool(row["cmf_valid"])
    assert row["cmf_upper"] == 101.0
    assert row["cmf_lower"] == 99.0


def test_flow_equal_to_threshold_holds_both_directions():
    up = last(frame(flat(6) + [(110.0, 100.0, 110.0, 0.5)]))
    assert up["cmf"] == 0.2
    assert int(up["signal"]) == 0
    down = last(frame(flat(6) + [(100.0, 90.0, 90.0, 0.5)]))
    assert down["cmf"] == -0.2
    assert int(down["signal"]) == 0


def test_continuation_and_late_flow_crossing_do_not_reenter():
    out = run(frame(flat(6) + [(110.0, 100.0, 110.0, 8.0), (112.0, 110.0, 112.0, 8.0)]), SMALL)
    assert out["signal"].tolist()[-2:] == [1, 0]
    assert out["cmf_reason"].iloc[-1] == "breakout_continuation"

    late = run(frame(flat(6) + [(110.0, 100.0, 110.0, 0.2), (112.0, 110.0, 112.0, 50.0)]), SMALL)
    assert late["cmf_reason"].iloc[-2] == "flow_below_threshold"
    assert late["cmf"].iloc[-1] > 0.2
    assert late["signal"].tolist()[-2:] == [0, 0]
    assert late["cmf_reason"].iloc[-1] == "breakout_continuation"

    short = run(frame(flat(6) + [(100.0, 90.0, 90.0, 8.0), (90.0, 88.0, 88.0, 8.0)]), SMALL)
    assert short["signal"].tolist()[-2:] == [-1, 0]
    assert short["cmf_reason"].iloc[-1] == "breakout_continuation"


def test_new_episode_after_breakout_ends_reenters():
    rows = flat(6) + [(110.0, 100.0, 110.0, 8.0)] + [(111.0, 109.0, 110.0, 1.0)] * 4
    rows += [(130.0, 110.0, 130.0, 20.0)]
    out = run(frame(rows), SMALL)
    assert out["signal"].iloc[6] == 1
    assert out["signal"].iloc[-1] == 1
    assert out["cmf_reason"].iloc[-1] == "entry_long"
    assert (out["signal"].iloc[7:-1] == 0).all()


def test_invalid_predecessor_never_manufactures_an_episode():
    rows = flat(10) + [(110.0, 100.0, 110.0, 8.0), (112.0, 110.0, 112.0, 8.0)]
    rows[5] = (101.0, 99.0, 100.0, float("nan"))
    out = run(frame(rows), SMALL)
    assert out["signal"].iloc[10] == 1
    clean = run(frame(flat(6) + [(101.0, 99.0, 100.0, float("nan"))] + flat(3)
                      + [(110.0, 100.0, 110.0, 8.0), (112.0, 110.0, 112.0, 8.0)]), SMALL)
    assert not bool(clean["cmf_valid"].iloc[10])
    assert clean["signal"].iloc[10] == 0
    assert clean["cmf_reason"].iloc[10] == "nonfinite_input"
    assert bool(clean["cmf_valid"].iloc[11])
    assert clean["signal"].iloc[11] == 0
    assert clean["cmf_reason"].iloc[11] == "breakout_continuation"


@pytest.mark.parametrize("mutate,reason", [
    (lambda r: (r[0], r[1], r[2], -1.0), "negative_volume"),
    (lambda r: (r[1], r[0], r[2], r[3]), "inconsistent_hlc"),
    (lambda r: (r[0], r[1], r[0] + 5.0, r[3]), "inconsistent_hlc"),
    (lambda r: (float("inf"), r[1], r[2], r[3]), "nonfinite_input"),
])
def test_malformed_bar_yields_reasoned_hold(mutate, reason):
    rows = flat(8)
    rows[-1] = mutate(rows[-1])
    row = last(frame(rows))
    assert int(row["signal"]) == 0
    assert not bool(row["cmf_valid"])
    assert row["cmf_reason"] == reason


def test_zero_volume_window_is_invalid_not_zero_flow():
    rows = flat(4) + [(101.0, 99.0, 100.0, 0.0)] * 2 + [(110.0, 100.0, 110.0, 0.0)]
    row = last(frame(rows))
    assert int(row["signal"]) == 0
    assert row["cmf_reason"] == "zero_volume_window"
    assert np.isnan(row["cmf"])


def test_zero_range_bar_contributes_zero_flow():
    rows = [(100.0, 100.0, 100.0, 5.0)] * 6
    out = run(frame(rows), SMALL)
    assert out["cmf"].iloc[-1] == 0.0
    assert (out["signal"] == 0).all()
    assert out["cmf_reason"].iloc[-1] == "no_breakout"


def test_insufficient_history_and_missing_columns():
    short = run(frame(flat(4)), SMALL)
    assert (short["cmf_reason"] == "insufficient_history").all()
    assert (short["signal"] == 0).all()
    missing = run(frame(flat(8)).drop(columns=["volume"]), SMALL)
    assert (missing["signal"] == 0).all()
    assert missing["cmf_reason"].iloc[-1] == "missing_columns:volume"


def test_timestamp_order_defect_detected():
    df = frame(flat(8))
    idx = list(df.index)
    idx[-1] = idx[-2]
    df.index = pd.DatetimeIndex(idx)
    row = last(df)
    assert row["cmf_reason"] == "timestamp_order"
    assert int(row["signal"]) == 0
    with_col = frame(flat(8)).reset_index(drop=True)
    with_col["timestamp"] = [1_000 * i for i in range(8)]
    with_col.loc[7, "timestamp"] = 3_000
    assert last(with_col)["cmf_reason"] == "timestamp_order"


@pytest.mark.parametrize("params", [
    {"flow_window": True},
    {"flow_window": 20.5},
    {"breakout_window": 1},
    {"breakout_window": 101},
    {"flow_threshold": 1.0},
    {"flow_threshold": -0.01},
    {"flow_threshold": float("nan")},
    {"flow_threshold": False},
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(frame(flat(30)), params)


def test_integral_float_window_accepted():
    out = run(frame(flat(30)), {"flow_window": 20.0})
    assert out["cmf_reason"].iloc[-1] == "no_breakout"


def _trend(n, step, seed):
    rng = np.random.RandomState(seed)
    close = 100.0 + step * np.arange(n) + rng.randn(n) * 0.2
    span = 0.6 + np.abs(rng.randn(n)) * 0.2
    if step > 0:
        high, low = close + span * 0.1, close - span
    else:
        high, low = close + span, close - span * 0.1
    vol = 100.0 + np.abs(rng.randn(n)) * 10
    df = pd.DataFrame({"open": close, "high": high, "low": low, "close": close, "volume": vol})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


def test_rising_series_only_longs_falling_only_shorts():
    up = run(_trend(200, 0.5, 1))
    assert (up["signal"] >= 0).all() and (up["signal"] == 1).any()
    down = run(_trend(200, -0.5, 2))
    assert (down["signal"] <= 0).all() and (down["signal"] == -1).any()


def _market(n=520, seed=1649):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 8 * np.sin(t / 30.0) + np.cumsum(rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.8 + 0.1
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    vol = 50 + np.abs(rng.randn(n)) * 40
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": vol})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


COLS = ["signal", "cmf", "cmf_upper", "cmf_lower", "cmf_breakout_state", "cmf_reason", "cmf_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [None, {"flow_window": 100, "breakout_window": 100, "flow_threshold": 0.0}])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = _market()
    full = run(df, params)
    p = {**_FUTURES.STRATEGY_REGISTRY[NAME]["default_params"], **(params or {})}
    need = max(p["flow_window"], p["breakout_window"] + 2)
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
    assert too_short["cmf_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_every_entry_is_reproducible_from_recorded_columns():
    df = _market()
    out = run(df)
    entries = out[out["signal"] != 0]
    assert len(entries) > 0
    for ts, row in entries.iterrows():
        i = df.index.get_loc(ts)
        win = df.iloc[i - 19: i + 1]
        mfm = np.where(win["high"] > win["low"],
                       (2 * win["close"] - win["high"] - win["low"]) / (win["high"] - win["low"]), 0.0)
        cmf = float((mfm * win["volume"]).sum() / win["volume"].sum())
        assert row["cmf"] == pytest.approx(cmf, rel=1e-12)
        assert row["cmf_upper"] == df["high"].iloc[i - 20: i].max()
        assert row["cmf_lower"] == df["low"].iloc[i - 20: i].min()
        assert row["cmf_eval_ts_ms"] == ts.value // 1_000_000
        if row["signal"] == 1:
            assert row["close"] > row["cmf_upper"] and row["cmf"] > row["cmf_threshold"]
        else:
            assert row["close"] < row["cmf_lower"] and row["cmf"] < -row["cmf_threshold"]
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


ENTRY = flat(6) + [(110.0, 100.0, 110.0, 8.0)]


def test_composer_flat_entry_and_open_position_blocks_duplicates_and_reversals():
    _, flat_decision = _compose(frame(ENTRY), "", params=SMALL)
    assert flat_decision["signal"] == 1 and flat_decision["open_action"] == "long"

    held = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, dup = _compose(frame(ENTRY), "long", held, params=SMALL)
    assert dup["open_action"] == "long" and dup["signal"] == 0

    short_ctx = {"side": "short", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, opposite = _compose(frame(ENTRY), "short", short_ctx, params=SMALL)
    assert opposite["close_fraction"] == 0.0 and opposite["signal"] == 0


def test_composer_close_wins_over_simultaneous_entry_and_survives_bad_entry_data():
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(frame(ENTRY), "long", due, params=SMALL)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1

    bad = ENTRY[:-1] + [(110.0, 100.0, 110.0, float("nan"))]
    evaluation, decision = _compose(frame(bad), "long", due, params=SMALL)
    assert evaluation.open_result_df["cmf_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(frame(flat(8)).drop(columns=["volume"]), "short", short_due, params=SMALL)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = _market(360, seed=7)
    full = run(df)
    need = 22
    for i in range(need - 1, len(df)):
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


