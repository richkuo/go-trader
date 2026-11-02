import contextlib
import csv
import gzip
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

from atr import ensure_atr_indicator
from backtester import Backtester
from closed_bar import FRAME_INTERVAL_ATTR, FRAME_TIMESTAMP_KIND_ATTR, declare_frame_cadence
from eval_windows import expand_sweep
import optimizer
from optimizer import DEFAULT_PARAM_RANGES, strategy_warmup_bars
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "trix_momentum"
STUDY = os.path.join(_REPO, "backtest", "candidates", "trix_momentum_1659")
DATA = os.path.join(_REPO, "backtest", "candidates", "chande_momentum_1661", "data")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
SEED = {"period": 18, "signal_period": 9, "recursion_length": 120}
SMALL = {"period": 3, "signal_period": 4, "recursion_length": 20}
BOUNDARY = {"period": 32, "signal_period": 9, "recursion_length": 197}
SMALLEST = {"period": 1, "signal_period": 1, "recursion_length": 6}
TINY = {"period": 1, "signal_period": 3, "recursion_length": 6}
DECISION_COLUMNS = ["trix_value", "trix_prev_value", "trix_signal_line", "trix_prev_signal_line", "trix_ema3",
                    "trix_ema3_base", "trix_diff", "trix_prev_diff", "trix_valid", "trix_value_valid",
                    "trix_reason_code", "signal"]


def _close_refs():
    with open(os.path.join(STUDY, "protocol.json")) as fh:
        return json.load(fh)["close_strategies"]


def _reg():
    return load_registry("futures")


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(p):
    return p["recursion_length"] + 2


def _declare(df, freq="4h", kind="open"):
    return declare_frame_cadence(df, freq, kind)


def _walk(n, seed, start=100.0, freq="4h", vol=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, vol, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return _declare(df, freq)


def _from_closes(closes, freq="4h"):
    c = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": c, "high": c * 1.001, "low": c * 0.999, "close": c, "volume": np.full(len(c), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(c), freq=freq)
    return _declare(df, freq)


def _frozen(symbol, tf, bars):
    with gzip.open(os.path.join(DATA, f"{symbol}_{tf}_candles.csv.gz"), "rt") as fh:
        rows = list(csv.reader(fh))[1:]
    df = pd.DataFrame(rows, columns=["timestamp"] + INPUT_COLUMNS).astype(
        {"timestamp": "int64", **{c: float for c in INPUT_COLUMNS}})
    df.index = pd.to_datetime(df["timestamp"], unit="ms")
    df = df[INPUT_COLUMNS].iloc[-bars:].copy()
    return _declare(df, tf)


def _ref_endpoint(c, t, p):
    a = 2.0 / (p["period"] + 1.0)
    b = 2.0 / (p["signal_period"] + 1.0)
    length = p["recursion_length"]
    run = [float(x) for x in c[t - length:t + 1]]
    e1 = e2 = e3 = run[0]
    third = [e3]
    for x in run[1:]:
        e1 = a * x + (1.0 - a) * e1
        e2 = a * e1 + (1.0 - a) * e2
        e3 = a * e2 + (1.0 - a) * e3
        third.append(e3)
    osc = [100.0 * (third[j] - third[j - 1]) / third[j - 1] for j in range(1, len(third))]
    line = osc[0]
    for o in osc[1:]:
        line = b * o + (1.0 - b) * line
    return osc[-1], line, third[-1], third[-2]


def _ref_signal(c, t, p):
    osc, line, _, _ = _ref_endpoint(c, t, p)
    posc, pline, _, _ = _ref_endpoint(c, t - 1, p)
    diff, prev = osc - line, posc - pline
    if diff > 0 and prev <= 0 and osc > 0:
        return 1, osc, line, posc, pline
    if diff < 0 and prev >= 0 and osc < 0:
        return -1, osc, line, posc, pline
    return 0, osc, line, posc, pline


def _check_grid_history_need_bounds():
    combos = [p for _, p in expand_sweep(SEED, [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])]
    assert max(_need(p) for p in combos) == 194


def test_values_match_an_independent_scalar_reference_and_both_entry_directions():
    df = _walk(260, 11, vol=0.03)
    c = df["close"].tolist()
    seen = set()
    for p in (SMALL, TINY, dict(SMALL, signal_period=1), dict(SMALL, period=2, recursion_length=12)):
        out = _apply(df, p)
        need = _need(p)
        assert (out["trix_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["trix_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert out["trix_value_valid"].iloc[need - 2] == 1.0
        assert out["trix_valid"].iloc[need - 1] == 1.0
        for t in range(need - 1, len(df)):
            sig, osc, line, posc, pline = _ref_signal(c, t, p)
            _, _, e3, base = _ref_endpoint(c, t, p)
            row = out.iloc[t]
            assert row["trix_value"] == pytest.approx(osc, rel=1e-9, abs=1e-12)
            assert row["trix_signal_line"] == pytest.approx(line, rel=1e-9, abs=1e-12)
            assert row["trix_prev_value"] == pytest.approx(posc, rel=1e-9, abs=1e-12)
            assert row["trix_prev_signal_line"] == pytest.approx(pline, rel=1e-9, abs=1e-12)
            assert row["trix_ema3"] == pytest.approx(e3, rel=1e-12)
            assert row["trix_ema3_base"] == pytest.approx(base, rel=1e-12)
            assert row["trix_seed_close"] == c[t - p["recursion_length"]]
            assert row["trix_prev_seed_close"] == c[t - 1 - p["recursion_length"]]
            assert row["trix_close"] == c[t] and row["trix_prev_close"] == c[t - 1]
            assert row["trix_diff"] == row["trix_value"] - row["trix_signal_line"]
            assert row["trix_prev_diff"] == row["trix_prev_value"] - row["trix_prev_signal_line"]
            if abs(osc - line) > 1e-9 and abs(posc - pline) > 1e-9 and abs(osc) > 1e-9:
                assert int(row["signal"]) == sig
            assert row["trix_reason_code"] == {1: 1.0, -1: -1.0, 0: row["trix_reason_code"]}[int(row["signal"])]
            seen.add(int(row["signal"]))
    assert seen == {-1, 0, 1}


def _last(closes, params):
    return _apply(_from_closes(closes), params).iloc[-1]


def _check_crossing_boundaries_and_the_zero_line_filter():
    p = TINY
    base = [100.0] * 20
    up = _last(base + [101.0], p)
    assert up["trix_prev_diff"] == 0.0 and up["trix_prev_value"] == 0.0
    assert up["trix_diff"] > 0 and up["trix_value"] > 0
    assert int(up["signal"]) == 1 and up["trix_reason"] == "entry_long"
    down = _last(base + [99.0], p)
    assert down["trix_prev_diff"] == 0.0 and down["trix_diff"] < 0 and down["trix_value"] < 0
    assert int(down["signal"]) == -1 and down["trix_reason"] == "entry_short"
    level = _apply(_from_closes(base + [100.0]), p)
    assert (level["signal"] == 0).all()
    assert level["trix_diff"].iloc[-1] == 0.0 and level["trix_reason"].iloc[-1] == "no_cross"
    q = SMALL
    rising = [100.0 * 1.01 ** i for i in range(60)]
    held = _apply(_from_closes(rising), q)
    assert (held["signal"].iloc[_need(q) - 1:] == 0).all()
    found = {}
    rng = np.random.default_rng(3)
    for trial in range(400):
        closes = list(100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.03, 60))))
        out = _apply(_from_closes(closes), q)
        for i in range(_need(q) - 1, len(out)):
            r = out.iloc[i]
            if r["trix_reason"] == "zero_line_disagrees":
                sign = 1 if r["trix_diff"] > 0 else -1
                assert r["trix_prev_diff"] * sign <= 0 and r["trix_value"] * sign <= 0
                assert int(r["signal"]) == 0 and r["trix_reason_code"] == 2.0
                found[sign] = True
        if len(found) == 2:
            break
    assert found == {1: True, -1: True}


def _check_sustained_conditions_do_not_repeat_entries():
    df = _walk(400, 21)
    out = _apply(df, SMALL)
    sig = out["signal"].to_numpy()
    for i in np.flatnonzero(sig != 0):
        assert sig[i - 1] != sig[i] or out["trix_prev_diff"].iloc[i] * sig[i] <= 0
    diff = out["trix_diff"].to_numpy()
    for i in np.flatnonzero(sig != 0):
        assert diff[i - 1] * sig[i] <= 0


def _holds_at_end(df, params, reason):
    before = df.copy()
    attrs = dict(df.attrs)
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    assert df.attrs == attrs
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["trix_valid"].iloc[-1] == 0.0
    assert out["trix_reason"].iloc[-1].split(":")[0] == reason, out["trix_reason"].iloc[-1]
    assert out["trix_reason_code"].iloc[-1] >= 10.0
    assert np.isnan(out["trix_value"].iloc[-1]) or reason in ("insufficient_history",)
    return out


def _set_at(df, pos, col, value):
    out = df.copy()
    out.iloc[pos, out.columns.get_loc(col)] = value
    return out


def _set_last(df, col, value):
    return _set_at(df, -1, col, value)


def _with_index(df, idx):
    out = df.set_axis(pd.DatetimeIndex(idx))
    out.attrs = dict(df.attrs)
    return out


INVALID_MARKET_INPUTS = [
    (lambda df: df.drop(columns=["close"]), "missing_columns"),
    (lambda df: df.drop(columns=["open", "high"]), "missing_columns"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [pd.NaT]), "missing_timestamps"),
    (lambda df: _set_last(df, "close", np.nan), "nonfinite_input"),
    (lambda df: _set_last(df, "high", np.inf), "nonfinite_input"),
    (lambda df: _set_last(df.astype({"close": object}), "close", "x"), "nonfinite_input"),
    (lambda df: _set_last(df, "close", 0.0), "nonpositive_price"),
    (lambda df: _set_last(df, "low", -1.0), "nonpositive_price"),
    (lambda df: _set_last(df, "open", df["high"].iloc[-1] * 2), "invalid_candle_geometry"),
    (lambda df: _set_last(df, "close", df["low"].iloc[-1] / 2), "invalid_candle_geometry"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-3]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=4)]), "cadence_gap"),
    (lambda df: df.drop(df.index[-10]), "cadence_gap"),
    (lambda df: df.iloc[-(_need(SMALL) - 1):], "insufficient_history"),
]


def _check_invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_MARKET_INPUTS):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(120, 3)), SMALL, reason)


def _check_negative_or_missing_volume_is_not_an_entry_input():
    df = _walk(120, 3)
    a = _apply(df, SMALL)
    b = _apply(_set_last(df, "volume", -1.0), SMALL)
    c = _apply(df.drop(columns=["volume"]), SMALL)
    for other in (b, c):
        pd.testing.assert_frame_equal(a[DECISION_COLUMNS], other[DECISION_COLUMNS])


def _check_missing_or_wrong_cadence_holds_every_row():
    df = _walk(120, 3)
    df.attrs.pop(FRAME_INTERVAL_ATTR)
    out = _apply(df, SMALL)
    assert (out["trix_reason"] == "missing_cadence").all() and (out["signal"] == 0).all()
    assert out["trix_value"].isna().all()
    wrong = _walk(120, 3)
    wrong.attrs[FRAME_INTERVAL_ATTR] = 3_600_000
    out = _apply(wrong, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["trix_reason"].iloc[_need(SMALL) - 1:]) == {"cadence_gap"}


def _check_a_defect_in_the_prior_endpoints_exclusive_oldest_bar_holds_the_decision():
    df = _walk(200, 5)
    p = SMALL
    t = 150
    oldest = t - p["recursion_length"] - 1
    bad = _apply(_set_at(df, oldest, "close", np.nan), p)
    clean = _apply(df, p)
    assert bad["trix_value_valid"].iloc[t] == 1.0
    assert bad["trix_value"].iloc[t] == clean["trix_value"].iloc[t]
    assert bad["trix_valid"].iloc[t] == 0.0 and int(bad["signal"].iloc[t]) == 0
    assert bad["trix_reason"].iloc[t] == "nonfinite_input"
    assert np.isnan(bad["trix_prev_value"].iloc[t])
    assert bad["trix_valid"].iloc[t + 1] == 1.0
    pd.testing.assert_series_equal(bad[DECISION_COLUMNS].iloc[t + 1], clean[DECISION_COLUMNS].iloc[t + 1],
                                   check_names=False)


def _check_overflowing_prices_hold_as_nonfinite_derived():
    closes = [1e308 if i % 2 == 0 else 1.7e308 for i in range(60)]
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _holds_at_end(df, SMALL, "nonfinite_derived")
    assert (out["signal"] == 0).all()
    assert out["trix_value"].isna().all()


def _check_subnormal_prices_hold_as_invalid_denominator():
    closes = [5e-324] * 60
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _apply(df, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["trix_reason"].iloc[_need(SMALL) - 1:]) <= {"invalid_denominator", "nonfinite_derived", "no_cross"}


def test_input_defects_hold_with_a_reason_and_preserve_the_frame(subtests):
    for label, check in (
        ("invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame", _check_invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame),
        ("negative_or_missing_volume_is_not_an_entry_input", _check_negative_or_missing_volume_is_not_an_entry_input),
        ("missing_or_wrong_cadence_holds_every_row", _check_missing_or_wrong_cadence_holds_every_row),
        ("a_defect_in_the_prior_endpoints_exclusive_oldest_bar_holds_the_decision", _check_a_defect_in_the_prior_endpoints_exclusive_oldest_bar_holds_the_decision),
        ("overflowing_prices_hold_as_nonfinite_derived", _check_overflowing_prices_hold_as_nonfinite_derived),
        ("subnormal_prices_hold_as_invalid_denominator", _check_subnormal_prices_hold_as_invalid_denominator),
    ):
        with subtests.test(case=label):
            check()


def _check_constant_prices_never_enter_and_report_finite_zero_values():
    out = _apply(_from_closes([250.0] * 200), SEED)
    assert (out["signal"] == 0).all()
    valid = out[out["trix_valid"] == 1.0]
    assert len(valid) == 200 - _need(SEED) + 1
    assert (valid["trix_reason"] == "no_cross").all()
    assert (valid["trix_value"] == 0.0).all() and (valid["trix_signal_line"] == 0.0).all()


def _check_rising_and_falling_series_report_the_matching_oscillator_sign():
    up = _apply(_from_closes([100.0 * 1.002 ** i for i in range(200)]), SEED)
    down = _apply(_from_closes([100.0 * 0.998 ** i for i in range(200)]), SEED)
    assert (up["trix_value"].dropna() > 0).all() and (down["trix_value"].dropna() < 0).all()
    assert (up["signal"] == 0).all() and (down["signal"] == 0).all()


def _check_a_defect_holds_only_inside_its_dependency_window():
    df = _walk(300, 5)
    need = _need(SMALL)
    bad = 150
    corrupt = _set_at(df, bad, "close", np.nan)
    out = _apply(corrupt, SMALL)
    clean = _apply(df, SMALL)
    assert (out["trix_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[:bad], clean[DECISION_COLUMNS].iloc[:bad])
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[bad + need:], clean[DECISION_COLUMNS].iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, SMALL)
    assert (out["trix_reason"].iloc[bad:bad + need - 1] == "cadence_gap").all()
    assert out["trix_valid"].iloc[bad + need - 1] == 1.0


INVALID_PARAMS = [
    {"period": 0}, {"period": -1}, {"period": 9.5}, {"period": True}, {"period": float("nan")}, {"period": "18"},
    {"period": 33, "recursion_length": 197}, {"period": 21}, {"signal_period": 0}, {"signal_period": False},
    {"signal_period": 120}, {"signal_period": 121}, {"signal_period": float("inf")}, {"recursion_length": 198},
    {"recursion_length": 107}, {"recursion_length": 120.5}, {"recursion_length": None},
    {"period": 1, "recursion_length": 5, "signal_period": 1},
]


BOUNDARY_PARAMS = [SMALLEST, BOUNDARY, {"period": 32, "signal_period": 191, "recursion_length": 192},
                   {"period": 1, "signal_period": 5, "recursion_length": 6}]


def _check_boundary_parameters_validate_and_decide_at_l_plus_two_rows():
    for params in BOUNDARY_PARAMS:
        with _case(params):
            _boundary_parameters_validate_and_decide_at_l_plus_two_rows(params)


def test_history_need_is_bounded_for_the_grid_and_every_boundary_parameter(subtests):
    for label, check in (
        ("grid_history_need_bounds", _check_grid_history_need_bounds),
        ("boundary_parameters_validate_and_decide_at_l_plus_two_rows", _check_boundary_parameters_validate_and_decide_at_l_plus_two_rows),
    ):
        with subtests.test(case=label):
            check()


def _boundary_parameters_validate_and_decide_at_l_plus_two_rows(params):
    reg = _reg()
    reg._registry.validate_params(NAME, params)
    df = _walk(260, 2)
    out = _apply(df, params)
    need = _need(params)
    assert out["trix_valid"].iloc[need - 2] == 0.0 and out["trix_reason"].iloc[need - 2] == "insufficient_history"
    assert out["trix_valid"].iloc[need - 1] == 1.0
    exact = _apply(df.iloc[:need], params)[DECISION_COLUMNS].iloc[-1]
    pd.testing.assert_series_equal(exact, out[DECISION_COLUMNS].iloc[need - 1], check_names=False)
    floats = {k: float(v) for k, v in params.items()}
    pd.testing.assert_frame_equal(_apply(df, floats), out)
    assert _need(BOUNDARY) == 199


def test_walk_forward_warmup_keeps_the_decision_before_the_first_scored_bar(monkeypatch):
    grid = optimizer.generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert strategy_warmup_bars(NAME, grid, DEFAULT_PARAM_RANGES[NAME]) == max(_need(p) for p in grid) == 194
    ranges = {"period": [18], "signal_period": [9], "recursion_length": [144, 192]}
    combos = optimizer.generate_param_grid(ranges)
    warmup = strategy_warmup_bars(NAME, combos, ranges)
    assert warmup == 194
    df = _walk(1500, 5, freq="1h")
    full = {p["recursion_length"]: _apply(df, p)["signal"] for p in combos}
    b = next(i for i in range(warmup, len(df)) if full[144].iloc[i] != 0 and full[192].iloc[i] == full[144].iloc[i])
    df = df.iloc[:2 * (b + 1)]
    first = {}
    original = optimizer.Backtester.run

    def record(self, signals_df, *args, **kwargs):
        p = kwargs.get("params") or {}
        if signals_df.index[0] == df.index[b]:
            first.setdefault(p["recursion_length"], int(signals_df["signal"].iloc[0]))
        return original(self, signals_df, *args, **kwargs)

    monkeypatch.setattr(optimizer.Backtester, "run", record)
    optimizer.walk_forward_optimize(df, NAME, ranges, n_splits=2, timeframe="1h", registry="futures",
                                    platform="hyperliquid", verbose=False, close_strategies=_close_refs(),
                                    direction="long")
    assert first == {144: int(full[144].iloc[b]), 192: int(full[192].iloc[b])}
    assert 0 not in first.values()


def test_timestamp_column_and_native_close_frames_decide_the_same_way():
    df = _walk(260, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = df.index.as_unit("ms").asi8.astype("int64")
    plain.attrs = dict(df.attrs)
    a = _apply(df, SMALL)
    b = _apply(plain, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
    np.testing.assert_array_equal(a["trix_value"].to_numpy(), b["trix_value"].to_numpy())
    native = df.set_axis(df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1))
    _declare(native, "4h", "native_close_inclusive")
    c = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), c["signal"].to_numpy())
    np.testing.assert_array_equal(a["trix_eval_ts_ms"].to_numpy(), c["trix_eval_ts_ms"].to_numpy())
    assert a["trix_eval_ts_ms"].iloc[-1] == df.index[-1].value // 1_000_000
    native.attrs.pop(FRAME_TIMESTAMP_KIND_ATTR)
    d = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), d["signal"].to_numpy())
    assert d["trix_eval_ts_ms"].isna().all()


def _check_full_series_every_prefix_and_every_200_row_frame_agree_on_frozen_candles():
    df = _frozen("BTC", "1h", 460)
    for p in (SEED, BOUNDARY, {"period": 12, "signal_period": 12, "recursion_length": 192}):
        need = _need(p)
        full = _apply(df, p)
        assert full["trix_valid"].iloc[need - 1:].all()
        for end in range(need, len(df) + 1):
            prefix = _apply(df.iloc[:end], p)[DECISION_COLUMNS].iloc[-1]
            pd.testing.assert_series_equal(prefix, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            if end >= 200:
                tail = _apply(df.iloc[end - 200:end], p)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(tail, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            span = _apply(df.iloc[end - need:end], p)[DECISION_COLUMNS].iloc[-1]
            pd.testing.assert_series_equal(span, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
        short = _apply(df.iloc[len(df) - need + 1:], p)
        assert short["trix_reason"].iloc[-1] == "insufficient_history"


def _check_frozen_candles_produce_both_directions_with_the_seed():
    out = _apply(_frozen("SOL", "4h", 1500), SEED)
    assert {-1, 1} <= set(out["signal"].unique())


def test_entry_rule_cases_fire_once_and_hold_with_their_reasons(subtests):
    for label, check in (
        ("crossing_boundaries_and_the_zero_line_filter", _check_crossing_boundaries_and_the_zero_line_filter),
        ("sustained_conditions_do_not_repeat_entries", _check_sustained_conditions_do_not_repeat_entries),
        ("constant_prices_never_enter_and_report_finite_zero_values", _check_constant_prices_never_enter_and_report_finite_zero_values),
        ("rising_and_falling_series_report_the_matching_oscillator_sign", _check_rising_and_falling_series_report_the_matching_oscillator_sign),
        ("frozen_candles_produce_both_directions_with_the_seed", _check_frozen_candles_produce_both_directions_with_the_seed),
    ):
        with subtests.test(case=label):
            check()


def _check_rows_outside_the_dependency_window_cannot_change_a_decision():
    df = _frozen("ETH", "4h", 400)
    p = SEED
    need = _need(p)
    t = 380
    full = _apply(df, p)
    early = df.copy()
    for col in ("open", "high", "low", "close"):
        early.iloc[: t - need + 1, early.columns.get_loc(col)] *= 1.5
    future = df.copy()
    future.iloc[t + 1:, future.columns.get_loc("close")] = np.nan
    for mutated in (early, future):
        got = _apply(mutated, p)
        pd.testing.assert_series_equal(got[DECISION_COLUMNS].iloc[t], full[DECISION_COLUMNS].iloc[t],
                                       check_names=False)
    recovered = _apply(_set_at(df, t - need, "close", np.nan), p)
    pd.testing.assert_series_equal(recovered[DECISION_COLUMNS].iloc[t], full[DECISION_COLUMNS].iloc[t],
                                   check_names=False)
    assert recovered["trix_valid"].iloc[t - 1] == 0.0


def test_decisions_depend_only_on_the_dependency_window(subtests):
    for label, check in (
        ("a_defect_holds_only_inside_its_dependency_window", _check_a_defect_holds_only_inside_its_dependency_window),
        ("full_series_every_prefix_and_every_200_row_frame_agree_on_frozen_candles", _check_full_series_every_prefix_and_every_200_row_frame_agree_on_frozen_candles),
        ("rows_outside_the_dependency_window_cannot_change_a_decision", _check_rows_outside_the_dependency_window_cannot_change_a_decision),
    ):
        with subtests.test(case=label):
            check()


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=_close_refs(), stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_partials_no_reversal_or_scale_in_and_long_only_opens_no_short():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, SMALL))
    with _case("test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in"):
        _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig)
    with _case("test_simulator_long_only_never_opens_a_short"):
        _simulator_long_only_never_opens_a_short(sig)


def _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig):
    bt = _bt(SMALL)
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"} and {"sl", "tiered_tp_pct"} <= reasons
    by_entry = {}
    for t in trades:
        by_entry.setdefault((t["entry_date"], t["side"]), []).append(t)
    spans = []
    for (entry_date, side), legs in by_entry.items():
        pos = sig.index.get_loc(pd.Timestamp(entry_date))
        want = 1 if side == "long" else -1
        assert pos > 0 and int(sig["signal"].iloc[pos - 1]) == want
        opened = float(df["open"].iloc[pos])
        assert legs[0]["entry_price"] == pytest.approx(opened * (1 + want * bt.slippage_pct), rel=1e-12)
        spans.append((pd.Timestamp(entry_date), max(pd.Timestamp(l["exit_date"]) for l in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    assert any(len(legs) > 1 for legs in by_entry.values())
    skipped = [i for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
               if not any(pd.Timestamp(e) == sig.index[min(i + 1, len(sig) - 1)] for e, _ in by_entry)]
    assert skipped


def _simulator_long_only_never_opens_a_short(sig):
    assert (sig["signal"] == -1).any()
    res = _bt(SMALL, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


_CACHE_SCRIPT = r"""
import json, sys
sys.path.insert(0, sys.argv[2])
import run_study as rs
proto = rs.load_protocol()
_, source = rs.verify_frozen(proto)
drop = json.loads(sys.argv[3])
rs.build_cache(sys.argv[1], proto, source, drop_funding={k: tuple(v) for k, v in drop.items()} or None)
rs.forbid_network()
proto["symbols"] = ["BTC/USDT"]
proto["windows"]["probe"] = ["2025-03-01", "2025-04-01"]
proto["held_out_windows"] = ["probe"]
runner = rs.Runner(proto)
sel = {"candidate_both": proto["seed_params"], "candidate_long": proto["seed_params"],
       "macd": {"fast_period": 12, "slow_period": 26, "signal_period": 9},
       "tema_cross": {"short_period": 5, "mid_period": 13, "long_period": 34}}
g = {"selections": {a: {"how": "selected", "params": p} for a, p in sel.items()}}
g["held_out"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe")} for a in proto["arms"]}
g["held_out_x2_slippage"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe", slippage_mult=2.0)}
                             for a in proto["candidate_arms"]}
out = {"verdict": rs.group_verdict(proto, g, True, True),
       "legs": {a: g["held_out"][a]["probe"]["legs"] for a in proto["arms"]}}
print(json.dumps(out, default=str))
"""


def _subdir(tmp_path, name):
    path = tmp_path / name
    path.mkdir()
    return path


def _cache_case(tmp_path, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {})],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _check_missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
    lo = int(pd.Timestamp("2025-03-01").timestamp() * 1000)
    hi = int(pd.Timestamp("2025-04-01").timestamp() * 1000)
    out = _cache_case(tmp_path, {"BTC": [lo, hi]})
    verdict = out["verdict"]
    assert verdict["verdict"] == "inconclusive"
    assert verdict["gates"]["all_windows_scorable"] is False
    bad = verdict["checks"]["unscorable_windows"]["probe"]
    for label in ("candidate_both BTC/USDT 1h", "candidate_long BTC/USDT 1h", "candidate_both x2 slippage BTC/USDT 1h",
                  "macd BTC/USDT 1h", "tema_cross BTC/USDT 1h", "incumbent median BTC/USDT 1h"):
        assert label in bad, bad
    funding = out["legs"]["candidate_both"]["BTC/USDT 1h"]["funding"]
    assert funding["unpriced_held_hours"] > 0 and funding["mode"] == "charge"
    assert 0 <= funding["priced_hours"] < funding["expected_hours"]
    assert funding["coverage_complete"] is False


def _check_complete_funding_gives_scorable_legs_and_long_only_comparators(tmp_path):
    out = _cache_case(tmp_path)
    verdict = out["verdict"]
    assert verdict["gates"]["all_windows_scorable"] is True
    for arm in ("candidate_long", "macd", "tema_cross"):
        assert verdict["checks"]["short_positions"][arm] == 0
    for arm, legs in out["legs"].items():
        leg = legs["BTC/USDT 1h"]
        assert leg["funding"]["mode"] == "charge" and leg["funding_incomplete"] is False
        assert leg["warmup"]["bars"] == 300
        assert leg["next_bar_fills"]["mismatches"] == 0


def test_study_funding_coverage_decides_whether_every_leg_is_scorable(subtests, tmp_path):
    for label, check in (
        ("missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive", lambda: _check_missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive(_subdir(tmp_path, "missing"))),
        ("complete_funding_gives_scorable_legs_and_long_only_comparators", lambda: _check_complete_funding_gives_scorable_legs_and_long_only_comparators(_subdir(tmp_path, "complete"))),
    ):
        with subtests.test(case=label):
            check()
