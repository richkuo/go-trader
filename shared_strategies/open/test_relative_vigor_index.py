import math
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


@pytest.mark.parametrize("period", [2, 3, 10, 30])
def test_real_registry_matches_independent_fixture(period):
    df = market()
    out = run(df, {"period": period})
    rvi, sig, events, edge = reference(df, period)
    assert sum(1 for e in events if e) > 0
    for t in range(len(df)):
        if rvi[t] is None:
            assert math.isnan(out["rvi"].iloc[t])
        else:
            assert abs(out["rvi"].iloc[t] - float(rvi[t])) <= TOLERANCE
        if sig[t] is None:
            assert math.isnan(out["rvi_signal"].iloc[t])
        else:
            assert abs(out["rvi_signal"].iloc[t] - float(sig[t])) <= TOLERANCE
    compared = [t for t in range(len(df)) if not edge[t]]
    assert len(compared) >= len(df) - 2
    assert [int(out["signal"].iloc[t]) for t in compared] == [events[t] for t in compared]


def test_initialization_bar_counts():
    df = market(60)
    out = run(df, SMALL)
    assert np.isnan(out["rvi"].iloc[4]) and np.isfinite(out["rvi"].iloc[5])
    assert np.isnan(out["rvi_signal"].iloc[7]) and np.isfinite(out["rvi_signal"].iloc[8])
    assert not out["rvi_valid"].iloc[NEED_SMALL - 2]
    assert out["rvi_valid"].iloc[NEED_SMALL - 1]
    assert (out["rvi_reason"].iloc[: NEED_SMALL - 1] == "insufficient_history").all()
    assert (out["signal"].iloc[: NEED_SMALL - 1] == 0).all()


def test_mirrored_input_mirrors_every_decision():
    df = market()
    up = run(df, SMALL)
    down = run(mirror(df), SMALL)
    assert (up["signal"] != 0).sum() > 0
    assert down["signal"].tolist() == [-s for s in up["signal"].tolist()]
    valid = up["rvi"].notna()
    assert np.array_equal(down["rvi"][valid].to_numpy(), -up["rvi"][valid].to_numpy())


def test_previous_zero_difference_then_strict_cross_emits_once():
    out = run(frame(doji(12) + [bar(100.0, 104.0), bar(104.0, 107.0)]), SMALL)
    assert out["rvi_diff"].iloc[11] == 0.0
    assert out["signal"].iloc[12] == 1 and out["rvi_reason"].iloc[12] == "entry_long"
    assert out["rvi_prev_diff"].iloc[12] == 0.0
    assert out["rvi_diff"].iloc[13] > 0
    assert out["signal"].iloc[13] == 0 and out["rvi_reason"].iloc[13] == "no_cross"

    short = run(frame(doji(12) + [bar(100.0, 96.0), bar(96.0, 93.0)]), SMALL)
    assert short["signal"].iloc[12] == -1 and short["rvi_reason"].iloc[12] == "entry_short"
    assert short["signal"].iloc[13] == 0


def test_constant_input_and_exact_equality_never_cross():
    out = run(frame(doji(40)), SMALL)
    assert (out["signal"] == 0).all()
    assert (out["rvi_diff"].iloc[NEED_SMALL - 1:] == 0.0).all()
    assert (out["rvi_reason"].iloc[NEED_SMALL - 1:] == "no_cross").all()


def test_zero_line_filter_blocks_cross_on_wrong_side_and_ablation_allows_it():
    rows = [bar(100.0 - 3 * i, 97.0 - 3 * i) for i in range(14)]
    rows.append(bar(58.0, 57.0))
    on = run(frame(rows), SMALL)
    assert on["rvi"].iloc[-1] < 0 and on["rvi_diff"].iloc[-1] > 0
    assert on["signal"].iloc[-1] == 0 and on["rvi_reason"].iloc[-1] == "zero_line_filter"
    assert bool(on["rvi_zero_line_filter"].iloc[-1])
    off = run(frame(rows), {"period": 3, "zero_line_filter": False})
    assert off["signal"].iloc[-1] == 1 and off["rvi_reason"].iloc[-1] == "entry_long"


def test_zero_range_windows_hold_and_zero_range_candle_is_allowed():
    flat = run(frame([(100.0, 100.0, 100.0, 100.0)] * 20 + [bar(100.0, 104.0)]), SMALL)
    assert (flat["rvi_reason"].iloc[NEED_SMALL - 1:-1] == "zero_range_window").all()
    assert (flat["signal"] == 0).all()
    assert np.isnan(flat["rvi"].iloc[15])

    rows = doji(12) + [(100.0, 100.0, 100.0, 100.0)] + [bar(100.0, 104.0)]
    mixed = run(frame(rows), SMALL)
    assert bool(mixed["rvi_valid"].iloc[12]) and mixed["rvi_reason"].iloc[12] == "no_cross"
    assert mixed["signal"].iloc[13] == 1


@pytest.mark.parametrize("mutate,reason", [
    (lambda r: (r[0], r[2], r[1], r[3]), "inconsistent_ohlc"),
    (lambda r: (r[1] + 5.0, r[1], r[2], r[3]), "inconsistent_ohlc"),
    (lambda r: (r[0], r[1], r[2], r[2] - 5.0), "inconsistent_ohlc"),
    (lambda r: (r[0], float("inf"), r[2], r[3]), "nonfinite_input"),
    (lambda r: (float("nan"), r[1], r[2], r[3]), "nonfinite_input"),
])
def test_malformed_candle_holds_every_window_that_contains_it(mutate, reason):
    df = market(80)
    bad_at = 40
    rows = df[["open", "high", "low", "close"]].to_numpy().tolist()
    rows[bad_at] = list(mutate(tuple(rows[bad_at])))
    bad = df.copy()
    bad[["open", "high", "low", "close"]] = rows
    out = run(bad, SMALL)
    clean = run(df, SMALL)
    for i in range(len(df)):
        if bad_at <= i <= bad_at + NEED_SMALL - 1:
            assert not out["rvi_valid"].iloc[i]
            assert out["signal"].iloc[i] == 0
            assert out["rvi_reason"].iloc[i] == reason
        else:
            assert out["signal"].iloc[i] == clean["signal"].iloc[i]
            assert out["rvi_reason"].iloc[i] == clean["rvi_reason"].iloc[i]


def test_timestamp_defects_hold():
    df = market(40)
    idx = list(df.index)
    idx[30] = idx[29]
    dup = df.copy()
    dup.index = pd.DatetimeIndex(idx)
    assert run(dup, SMALL)["rvi_reason"].iloc[30] == "timestamp_order"

    idx = list(df.index)
    idx[30], idx[31] = idx[31], idx[30]
    rev = df.copy()
    rev.index = pd.DatetimeIndex(idx)
    assert run(rev, SMALL)["rvi_reason"].iloc[31] == "timestamp_order"

    idx = list(df.index)
    idx[30] = pd.NaT
    nat = df.copy()
    nat.index = pd.DatetimeIndex(idx)
    out = run(nat, SMALL)
    assert out["rvi_reason"].iloc[30] == "timestamp_order"
    assert (out["signal"].iloc[30:30 + NEED_SMALL] == 0).all()

    bare = df.reset_index(drop=True)
    out = run(bare, SMALL)
    assert (out["rvi_reason"] == "missing_timestamps").all() and (out["signal"] == 0).all()

    col = df.reset_index(drop=True)
    col["timestamp"] = (df.index.asi8 // 1_000_000)
    assert run(col, SMALL)["signal"].tolist() == run(df, SMALL)["signal"].tolist()


def test_cadence_gap_invalidates_exactly_the_windows_that_contain_it():
    df = market(120)
    gap_at = 60
    gapped = df.drop(df.index[gap_at])
    out = run(gapped, SMALL)
    for i in range(len(gapped)):
        contains = i - NEED_SMALL + 2 <= gap_at <= i
        if contains:
            assert not out["rvi_valid"].iloc[i]
            assert out["rvi_reason"].iloc[i] == "cadence_gap"
            assert out["signal"].iloc[i] == 0
        elif i >= NEED_SMALL - 1:
            assert out["rvi_valid"].iloc[i]
    reset = gap_at + NEED_SMALL - 1
    expected = run(gapped.iloc[gap_at:], SMALL)
    assert out["signal"].iloc[reset:].tolist() == expected["signal"].iloc[NEED_SMALL - 1:].tolist()


def test_appending_a_finer_step_never_reclassifies_earlier_bars():
    coarse = market(30, step="8h")
    out_before = run(coarse, SMALL)
    extra = market(31, seed=9, step="8h").iloc[-1:]
    extra.index = [coarse.index[-1] + pd.Timedelta(hours=4)]
    longer = pd.concat([coarse, extra])
    out_after = run(longer, SMALL)
    cols = ["signal", "rvi_reason", "rvi_valid"]
    assert out_after[cols].iloc[:30].equals(out_before[cols])
    assert out_after["rvi_reason"].iloc[30] == "cadence_gap"
    assert bool(out_before["rvi_valid"].iloc[-1])


@pytest.mark.parametrize("params", [
    {"period": True},
    {"period": 10.5},
    {"period": 1},
    {"period": 101},
    {"period": float("nan")},
    {"period": "10"},
    {"zero_line_filter": 1},
    {"zero_line_filter": "true"},
    {"zero_line_filter": None},
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(market(40), params)


def test_integral_float_period_and_numpy_bool_accepted():
    a = run(market(60), {"period": 4.0, "zero_line_filter": np.bool_(True)})
    b = run(market(60), {"period": 4, "zero_line_filter": True})
    assert a["signal"].tolist() == b["signal"].tolist()


def test_missing_columns_hold():
    out = run(market(40).drop(columns=["open"]), SMALL)
    assert (out["signal"] == 0).all()
    assert out["rvi_reason"].iloc[-1] == "missing_columns:open"


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


def test_rising_series_only_longs_falling_only_shorts():
    up = run(trend(200, 2.0, 1))
    assert (up["signal"] >= 0).all() and (up["signal"] == 1).any()
    down = run(trend(200, -2.0, 2))
    assert (down["signal"] <= 0).all() and (down["signal"] == -1).any()


COLS = ["signal", "rvi", "rvi_signal", "rvi_diff", "rvi_prev_diff", "rvi_reason", "rvi_valid"]


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [None, {"period": 100, "zero_line_filter": False}])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = market(420)
    df.iloc[150, df.columns.get_loc("high")] = np.nan
    df = df.drop(df.index[260])
    full = run(df, params)
    p = {**_FUTURES.STRATEGY_REGISTRY[NAME]["default_params"], **(params or {})}
    need = p["period"] + 7
    assert (full["signal"] != 0).sum() > 0
    for i in range(len(df)):
        prefix = run(df.iloc[: i + 1], params).iloc[-1]
        _same(full.iloc[i], prefix)
        if i + 1 >= need:
            tail = run(df.iloc[max(0, i - 199): i + 1], params).iloc[-1]
            _same(full.iloc[i], tail)
    exact = run(df.iloc[len(df) - need:], params).iloc[-1]
    _same(full.iloc[-1], exact)
    too_short = run(df.iloc[len(df) - need + 1:], params).iloc[-1]
    assert too_short["rvi_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_every_entry_is_reproducible_from_recorded_columns():
    df = market()
    original = df.copy()
    out = run(df)
    entries = out[out["signal"] != 0]
    assert len(entries) > 0
    for ts, row in entries.iterrows():
        assert row["rvi"] == pytest.approx(row["rvi_num_mean"] / row["rvi_den_mean"], rel=TOLERANCE)
        assert row["rvi_diff"] == pytest.approx(row["rvi"] - row["rvi_signal"], rel=TOLERANCE, abs=TOLERANCE)
        assert row["rvi_eval_ts_ms"] == ts.value // 1_000_000
        assert bool(row["rvi_valid"])
        if row["signal"] == 1:
            assert row["rvi_prev_diff"] <= 0 < row["rvi_diff"] and row["rvi"] > 0
        else:
            assert row["rvi_prev_diff"] >= 0 > row["rvi_diff"] and row["rvi"] < 0
    assert df.equals(original)


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


def test_production_hyperliquid_dataframe_is_evaluated_with_its_timestamps():
    df = _CHECK_HL._make_dataframe(_hl_candles(market(200)))
    direct = run(market(200))
    out = run(df)
    assert out["signal"].tolist() == direct["signal"].tolist()
    assert out["rvi_reason"].iloc[-1] != "missing_timestamps"


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


