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
    "_vortex_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_vortex_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_vortex_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

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


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/vortex_trend_1647/REPORT.md")
    assert entry["default_params"] == {"period": 14, "min_separation": 0.0}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


def test_flat_then_up_bar_values_and_long_entry():
    out = run(frame(flat(3) + [UP]), SMALL)
    row = out.iloc[-1]
    assert row["vortex_vi_plus"] == pytest.approx(6.0 / 5.0)
    assert row["vortex_vi_minus"] == pytest.approx(2.0 / 5.0)
    assert row["vortex_tr_sum"] == 5.0
    assert row["vortex_prev_diff"] == 0.0
    assert int(row["signal"]) == 1
    assert row["vortex_reason"] == "entry_long"
    assert bool(row["vortex_valid"])


def test_flat_then_down_bar_short_entry_mirror():
    row = run(frame(flat(3) + [DOWN]), SMALL).iloc[-1]
    assert row["vortex_vi_plus"] == pytest.approx(2.0 / 5.0)
    assert row["vortex_vi_minus"] == pytest.approx(6.0 / 5.0)
    assert int(row["signal"]) == -1
    assert row["vortex_reason"] == "entry_short"


def test_separation_not_met_and_equality_is_no_entry():
    df = frame(flat(3) + [UP])
    d = float(run(df, SMALL)["vortex_diff"].iloc[-1])
    above = run(df, {"period": 2, "min_separation": round(d + 0.05, 6)}).iloc[-1]
    assert int(above["signal"]) == 0
    assert above["vortex_reason"] == "separation_not_met"
    equal = run(df, {"period": 2, "min_separation": d}).iloc[-1]
    assert equal["vortex_diff"] == equal["vortex_separation"]
    assert int(equal["signal"]) == 0
    assert equal["vortex_reason"] == "separation_not_met"
    below = run(df, {"period": 2, "min_separation": round(d - 0.05, 6)}).iloc[-1]
    assert int(below["signal"]) == 1

    dn = frame(flat(3) + [DOWN])
    dd = float(run(dn, SMALL)["vortex_diff"].iloc[-1])
    eq_short = run(dn, {"period": 2, "min_separation": -dd}).iloc[-1]
    assert int(eq_short["signal"]) == 0
    assert eq_short["vortex_reason"] == "separation_not_met"


def test_continuation_does_not_reenter_and_one_bar_flip_reverses_event():
    out = run(frame(flat(3) + [UP, (105.0, 103.0, 104.0), (107.0, 105.0, 106.0)]), SMALL)
    assert out["signal"].tolist()[-3:] == [1, 0, 0]
    assert out["vortex_reason"].tolist()[-2:] == ["no_cross", "no_cross"]

    flip = run(frame(flat(3) + [UP, (105.0, 103.0, 104.0), (103.0, 95.0, 96.0)]), SMALL)
    assert flip["vortex_prev_diff"].iloc[-1] > 0
    assert flip["vortex_diff"].iloc[-1] < 0
    assert int(flip["signal"].iloc[-1]) == -1


def test_recross_of_separation_line_is_a_new_event():
    rng = np.random.RandomState(11)
    n = 600
    close = 100 + np.cumsum(rng.randn(n) * 0.5)
    open_ = np.r_[close[0], close[:-1]]
    span = np.abs(rng.randn(n)) * 0.5 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": 1.0},
                      index=pd.date_range("2026-01-01", periods=n, freq="4h"))
    sep = 0.05
    out = run(df, {"period": 14, "min_separation": sep})
    d = out["vortex_diff"].to_numpy()
    longs = np.flatnonzero(out["signal"].to_numpy() == 1)
    found = False
    for a, b in zip(longs[:-1], longs[1:]):
        if np.all(d[a:b + 1] > 0):
            found = True
            assert np.any(d[a + 1:b] <= sep)
    assert found


def test_matches_independent_reference_on_random_series():
    rng = np.random.RandomState(1647)
    n = 500
    close = 100 + np.cumsum(rng.randn(n))
    open_ = np.r_[close[0], close[:-1]]
    span = np.abs(rng.randn(n)) + 0.05
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": 1.0},
                      index=pd.date_range("2025-01-01", periods=n, freq="4h"))
    for period, sep in ((14, 0.0), (7, 0.05), (28, 0.1)):
        out = run(df, {"period": period, "min_separation": sep})
        vip, vim, d, sig = reference(df, period, sep)
        valid = out["vortex_valid"].to_numpy()
        assert not valid[: period + 1].any() and valid[period + 1:].all()
        np.testing.assert_allclose(out["vortex_vi_plus"].to_numpy()[period:],
                                   vip.to_numpy()[period:], rtol=1e-12)
        np.testing.assert_allclose(out["vortex_vi_minus"].to_numpy()[period:],
                                   vim.to_numpy()[period:], rtol=1e-12)
        assert (out["signal"].to_numpy()[period + 1:] == sig.to_numpy()[period + 1:]).all()
        assert (out["signal"] != 0).sum() > 0


def test_falling_then_rising_gives_exactly_one_long_and_mirror():
    fall = [(100.5 - i, 99.5 - i, 100.0 - i) for i in range(30)]
    rise = [(71.5 + i, 70.5 + i, 71.0 + i) for i in range(30)]
    out = run(frame(fall + rise), {"period": 14, "min_separation": 0.0})
    assert (out["signal"].iloc[:30] == 0).all()
    assert out["signal"].tolist().count(1) == 1
    assert out["signal"].tolist().count(-1) == 0
    assert out["vortex_reason"].iloc[16] == "no_cross"

    up = [(100.5 + i, 99.5 + i, 100.0 + i) for i in range(30)]
    down = [(129.5 - i, 128.5 - i, 129.0 - i) for i in range(30)]
    mirror = run(frame(up + down), {"period": 14, "min_separation": 0.0})
    assert mirror["signal"].tolist().count(-1) == 1
    assert mirror["signal"].tolist().count(1) == 0


def test_constant_prices_are_zero_true_range_not_a_reading():
    out = run(frame([(100.0, 100.0, 100.0)] * 8), SMALL)
    assert (out["signal"] == 0).all()
    assert out["vortex_reason"].iloc[-1] == "zero_true_range"
    assert np.isnan(out["vortex_vi_plus"].iloc[-1])
    assert out["vortex_tr_sum"].iloc[-1] == 0.0


def test_missing_previous_bar_masks_shared_true_range_fallback():
    rows = flat(8) + [UP]
    rows[3] = (101.0, 99.0, float("nan"))
    out = run(frame(rows), SMALL)
    assert out["vortex_reason"].iloc[3] == "nonfinite_input"
    assert np.isnan(out["vortex_vi_plus"].iloc[4])
    assert np.isnan(out["vortex_vi_plus"].iloc[5])
    for i in (4, 5, 6):
        assert not bool(out["vortex_valid"].iloc[i])
        assert out["vortex_reason"].iloc[i] == "nonfinite_input"
        assert int(out["signal"].iloc[i]) == 0
    assert bool(out["vortex_valid"].iloc[7])
    assert out["vortex_reason"].iloc[7] == "no_cross"
    assert int(out["signal"].iloc[8]) == 1
    first = run(frame(flat(4)), SMALL)
    assert np.isnan(first["vortex_vi_plus"].iloc[0])
    assert first["vortex_reason"].iloc[0] == "insufficient_history"


@pytest.mark.parametrize("mutate,reason", [
    (lambda r: (r[1], r[0], r[2]), "inconsistent_hlc"),
    (lambda r: (r[0], r[1], r[0] + 5.0), "inconsistent_hlc"),
    (lambda r: (float("inf"), r[1], r[2]), "nonfinite_input"),
    (lambda r: (r[0], float("nan"), r[2]), "nonfinite_input"),
])
def test_malformed_bar_yields_reasoned_hold(mutate, reason):
    rows = flat(7) + [UP]
    rows[-1] = mutate(rows[-1])
    row = run(frame(rows), SMALL).iloc[-1]
    assert int(row["signal"]) == 0
    assert not bool(row["vortex_valid"])
    assert row["vortex_reason"] == reason


def test_insufficient_history_boundary_and_missing_columns():
    exact = run(frame(flat(3) + [UP]), SMALL)
    assert bool(exact["vortex_valid"].iloc[-1])
    short = run(frame(flat(2) + [UP]), SMALL)
    assert (short["vortex_reason"] == "insufficient_history").all()
    assert (short["signal"] == 0).all()
    missing = run(frame(flat(8)).drop(columns=["high"]), SMALL)
    assert (missing["signal"] == 0).all()
    assert missing["vortex_reason"].iloc[-1] == "missing_columns:high"


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


def test_time_gap_uses_previous_row_like_shared_true_range():
    df = frame(flat(3) + [UP])
    gapped = df.copy()
    gapped.index = pd.DatetimeIndex(list(df.index[:3]) + [df.index[3] + pd.Timedelta(hours=12)])
    a, b = run(df, SMALL).iloc[-1], run(gapped, SMALL).iloc[-1]
    assert a["vortex_vi_plus"] == b["vortex_vi_plus"]
    assert int(b["signal"]) == 1


@pytest.mark.parametrize("params", [
    {"period": True},
    {"period": 14.5},
    {"period": 1},
    {"period": 101},
    {"min_separation": 1.0},
    {"min_separation": -0.01},
    {"min_separation": float("nan")},
    {"min_separation": False},
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(frame(flat(30)), params)


def test_integral_float_period_accepted():
    out = run(frame(flat(30)), {"period": 14.0})
    assert out["vortex_reason"].iloc[-1] == "no_cross"


def _market(n=520, seed=1647):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 8 * np.sin(t / 30.0) + np.cumsum(rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.8 + 0.1
    high = np.maximum(open_, close) + span
    low = np.minimum(open_, close) - span
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


COLS = ["signal", "vortex_vi_plus", "vortex_vi_minus", "vortex_diff", "vortex_prev_diff",
        "vortex_tr_sum", "vortex_reason", "vortex_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [None, {"period": 100, "min_separation": 0.05}])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = _market()
    full = run(df, params)
    p = {**_FUTURES.STRATEGY_REGISTRY[NAME]["default_params"], **(params or {})}
    need = p["period"] + 2
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
    assert too_short["vortex_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_every_entry_is_reproducible_from_recorded_columns():
    df = _market()
    out = run(df)
    entries = out[out["signal"] != 0]
    assert len(entries) > 0
    for ts, row in entries.iterrows():
        i = df.index.get_loc(ts)
        win = df.iloc[i - 14: i + 1]
        h, l, c = win["high"].to_numpy(), win["low"].to_numpy(), win["close"].to_numpy()
        vmp = np.abs(h[1:] - l[:-1]).sum()
        vmm = np.abs(l[1:] - h[:-1]).sum()
        tr = np.maximum.reduce([h[1:] - l[1:], np.abs(h[1:] - c[:-1]), np.abs(l[1:] - c[:-1])]).sum()
        assert row["vortex_tr_sum"] == pytest.approx(tr, rel=1e-12)
        assert row["vortex_vi_plus"] == pytest.approx(vmp / tr, rel=1e-12)
        assert row["vortex_vi_minus"] == pytest.approx(vmm / tr, rel=1e-12)
        assert row["vortex_prev_diff"] == out["vortex_diff"].iloc[i - 1]
        assert row["vortex_eval_ts_ms"] == ts.value // 1_000_000
        s = row["vortex_separation"]
        if row["signal"] == 1:
            assert row["vortex_diff"] > s >= row["vortex_prev_diff"]
        else:
            assert row["vortex_diff"] < -s <= row["vortex_prev_diff"]
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


ENTRY = flat(3) + [UP]


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

    bad = ENTRY[:-1] + [(103.0, 101.0, float("nan"))]
    evaluation, decision = _compose(frame(bad), "long", due, params=SMALL)
    assert evaluation.open_result_df["vortex_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(frame(flat(8)).drop(columns=["high"]), "short", short_due, params=SMALL)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = _market(360, seed=7)
    full = run(df)
    need = 16
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


