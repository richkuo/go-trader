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

from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
import optimizer
from optimizer import DEFAULT_PARAM_RANGES, strategy_warmup_bars
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "true_strength_index"
STUDY = os.path.join(_REPO, "backtest", "candidates", "true_strength_index_1667")
DATA = os.path.join(_REPO, "backtest", "candidates", "chande_momentum_1661", "data")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
SEED = {"slow_period": 25, "fast_period": 13, "signal_period": 7, "recursion_length": 180}
SMALL = {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 18}
BOUNDARY = {"slow_period": 33, "fast_period": 13, "signal_period": 9, "recursion_length": 198}
REL = 1e-10
ABS = 1e-12
DECISION_COLUMNS = ["tsi_value", "tsi_prev_value", "tsi_signal_line", "tsi_prev_signal_line", "tsi_diff",
                    "tsi_prev_diff", "tsi_slow_change", "tsi_double_change", "tsi_slow_abs", "tsi_double_abs",
                    "tsi_signal_seed_change", "tsi_valid", "tsi_value_valid", "tsi_reason_code", "signal"]


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
    return p["recursion_length"] + 1


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
    a = 2.0 / (p["slow_period"] + 1.0)
    b = 2.0 / (p["fast_period"] + 1.0)
    g = 2.0 / (p["signal_period"] + 1.0)
    length = p["recursion_length"]
    window = [float(x) for x in c[t - length + 1:t + 1]]
    changes = [window[i] - window[i - 1] for i in range(1, len(window))]
    slow = fast = changes[0]
    slow_abs = fast_abs = abs(changes[0])
    line = None
    seed_at = None
    tsi = None
    for j, d in enumerate(changes):
        if j > 0:
            slow = a * d + (1.0 - a) * slow
            fast = b * slow + (1.0 - b) * fast
            slow_abs = a * abs(d) + (1.0 - a) * slow_abs
            fast_abs = b * slow_abs + (1.0 - b) * fast_abs
        tsi = 100.0 * (fast / fast_abs) if fast_abs > 0 else None
        if line is None:
            if tsi is not None:
                line, seed_at = tsi, j
        else:
            line = g * tsi + (1.0 - g) * line
    return {"tsi": tsi, "line": line, "slow": slow, "fast": fast, "slow_abs": slow_abs, "fast_abs": fast_abs,
            "seed_at": seed_at, "first": changes[0]}


def _ref_signal(c, t, p):
    cur = _ref_endpoint(c, t, p)
    prev = _ref_endpoint(c, t - 1, p)
    diff, pdiff = cur["tsi"] - cur["line"], prev["tsi"] - prev["line"]
    if diff > 0 and pdiff <= 0 and cur["tsi"] > 0:
        return 1, cur, prev
    if diff < 0 and pdiff >= 0 and cur["tsi"] < 0:
        return -1, cur, prev
    return 0, cur, prev


def _approx(x):
    return pytest.approx(x, rel=REL, abs=ABS)


REFERENCE_PARAMS = [SMALL, dict(SMALL, signal_period=5), dict(SMALL, slow_period=4, fast_period=3,
                                                               recursion_length=24)]


def test_values_match_an_independent_scalar_reference_and_both_entry_directions(subtests):
    for params in REFERENCE_PARAMS:
        with subtests.test(params=params):
            _values_match_an_independent_scalar_reference_and_both_entry_directions(params)


def _values_match_an_independent_scalar_reference_and_both_entry_directions(params):
    df = _walk(260, 11, vol=0.03)
    c = df["close"].tolist()
    out = _apply(df, params)
    need = _need(params)
    assert (out["tsi_valid"].iloc[:need - 1] == 0.0).all()
    assert (out["tsi_reason"].iloc[:need - 1] == "insufficient_history").all()
    assert out["tsi_value_valid"].iloc[need - 2] == 1.0
    assert out["tsi_valid"].iloc[need - 1] == 1.0
    seen = set()
    for t in range(need - 1, len(df)):
        sig, cur, prev = _ref_signal(c, t, params)
        row = out.iloc[t]
        assert row["tsi_value"] == _approx(cur["tsi"])
        assert row["tsi_signal_line"] == _approx(cur["line"])
        assert row["tsi_prev_value"] == _approx(prev["tsi"])
        assert row["tsi_prev_signal_line"] == _approx(prev["line"])
        assert row["tsi_slow_change"] == _approx(cur["slow"])
        assert row["tsi_double_change"] == _approx(cur["fast"])
        assert row["tsi_slow_abs"] == _approx(cur["slow_abs"])
        assert row["tsi_double_abs"] == _approx(cur["fast_abs"])
        assert row["tsi_prev_double_abs"] == _approx(prev["fast_abs"])
        assert row["tsi_signal_seed_change"] == float(cur["seed_at"]) == 0.0
        assert row["tsi_first_change"] == cur["first"]
        assert row["tsi_seed_close"] == c[t - params["recursion_length"] + 1]
        assert row["tsi_prev_seed_close"] == c[t - params["recursion_length"]]
        assert row["tsi_close"] == c[t] and row["tsi_prev_close"] == c[t - 1]
        assert row["tsi_diff"] == row["tsi_value"] - row["tsi_signal_line"]
        assert row["tsi_prev_diff"] == row["tsi_prev_value"] - row["tsi_prev_signal_line"]
        assert row["tsi_value"] == 100.0 * (row["tsi_double_change"] / row["tsi_double_abs"])
        if (abs(cur["tsi"] - cur["line"]) > 1e-9 and abs(prev["tsi"] - prev["line"]) > 1e-9
                and abs(cur["tsi"]) > 1e-9):
            assert int(row["signal"]) == sig
        assert row["tsi_reason_code"] == {1: 1.0, -1: -1.0, 0: row["tsi_reason_code"]}[int(row["signal"])]
        seen.add(int(row["signal"]))
    assert seen == {-1, 0, 1}


def _last(closes, params):
    return _apply(_from_closes(closes), params).iloc[-1]


def test_zero_denominators_delay_the_signal_seed_and_equality_crossings_enter_both_ways():
    p = SMALL
    flat = [100.0] * 20
    long_ = _apply(_from_closes(flat + [99.0, 104.0]), p)
    seed_row, entry = long_.iloc[-2], long_.iloc[-1]
    assert long_["tsi_reason"].iloc[-3] == "invalid_denominator" and long_["tsi_value_valid"].iloc[-3] == 0.0
    assert seed_row["tsi_value_valid"] == 1.0 and seed_row["tsi_value"] == _approx(-100.0)
    assert seed_row["tsi_signal_line"] == seed_row["tsi_value"] and seed_row["tsi_diff"] == 0.0
    assert seed_row["tsi_signal_seed_change"] == float(p["recursion_length"] - 2)
    assert entry["tsi_prev_diff"] == 0.0 and entry["tsi_diff"] > 0 and entry["tsi_value"] > 0
    assert int(entry["signal"]) == 1 and entry["tsi_reason"] == "entry_long"
    assert entry["tsi_signal_seed_change"] == float(p["recursion_length"] - 3)
    c = _from_closes(flat + [99.0, 104.0])["close"].tolist()
    ref = _ref_endpoint(c, len(c) - 1, p)
    assert entry["tsi_value"] == _approx(ref["tsi"]) and entry["tsi_signal_line"] == _approx(ref["line"])
    short = _last(flat + [101.0, 96.0], p)
    assert short["tsi_prev_diff"] == 0.0 and short["tsi_diff"] < 0 and short["tsi_value"] < 0
    assert int(short["signal"]) == -1 and short["tsi_reason"] == "entry_short"
    against = _last(flat + [101.0, 100.8], p)
    assert against["tsi_prev_diff"] == 0.0 and against["tsi_diff"] < 0 and against["tsi_value"] > 0
    assert int(against["signal"]) == 0 and against["tsi_reason"] == "centerline_disagrees"
    assert against["tsi_reason_code"] == 2.0
    mirror = _last(flat + [99.0, 99.2], p)
    assert mirror["tsi_prev_diff"] == 0.0 and mirror["tsi_diff"] > 0 and mirror["tsi_value"] < 0
    assert int(mirror["signal"]) == 0 and mirror["tsi_reason"] == "centerline_disagrees"
    level = _last(flat + [101.0, 102.0], p)
    assert level["tsi_value"] == 100.0 and level["tsi_signal_line"] == 100.0 and level["tsi_diff"] == 0.0
    assert int(level["signal"]) == 0 and level["tsi_reason"] == "no_cross"


def test_constant_rising_and_falling_series_never_enter():
    flat = _apply(_from_closes([250.0] * 200), SEED)
    assert (flat["signal"] == 0).all()
    assert set(flat["tsi_reason"].iloc[_need(SEED) - 1:]) == {"invalid_denominator"}
    assert flat["tsi_value"].isna().all()
    up = _apply(_from_closes([100.0 * 1.002 ** i for i in range(260)]), SEED)
    down = _apply(_from_closes([100.0 * 0.998 ** i for i in range(260)]), SEED)
    for out, sign in ((up, 1.0), (down, -1.0)):
        valid = out[out["tsi_valid"] == 1.0]
        assert len(valid) == 260 - _need(SEED) + 1
        assert (valid["tsi_value"] == 100.0 * sign).all() and (valid["tsi_diff"] == 0.0).all()
        assert (out["signal"] == 0).all() and set(valid["tsi_reason"]) == {"no_cross"}


def test_leading_flat_history_inside_the_window_seeds_late_and_matches_the_reference():
    p = SMALL
    rng = np.random.default_rng(7)
    closes = [100.0] * 30 + list(100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.02, 40))))
    df = _from_closes(closes)
    out = _apply(df, p)
    c = df["close"].tolist()
    first_move = 30
    for t in range(first_move, len(c)):
        if t - p["recursion_length"] + 2 >= first_move or t < _need(p) - 1:
            continue
        row = out.iloc[t]
        ref = _ref_endpoint(c, t, p)
        assert row["tsi_value_valid"] == 1.0
        assert row["tsi_signal_seed_change"] == float(ref["seed_at"]) > 0
        assert row["tsi_value"] == _approx(ref["tsi"]) and row["tsi_signal_line"] == _approx(ref["line"])


def test_sustained_conditions_do_not_repeat_entries():
    out = _apply(_walk(400, 21), SMALL)
    sig = out["signal"].to_numpy()
    diff = out["tsi_diff"].to_numpy()
    assert (sig != 0).sum() > 5
    for i in np.flatnonzero(sig != 0):
        assert diff[i - 1] * sig[i] <= 0
        assert sig[i - 1] != sig[i] or out["tsi_prev_diff"].iloc[i] * sig[i] <= 0


def _holds_at_end(df, params, reason):
    before = df.copy()
    attrs = dict(df.attrs)
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    assert df.attrs == attrs
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["tsi_valid"].iloc[-1] == 0.0
    assert out["tsi_reason"].iloc[-1].split(":")[0] == reason, out["tsi_reason"].iloc[-1]
    assert out["tsi_reason_code"].iloc[-1] >= 10.0
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


def _invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_MARKET_INPUTS):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(120, 3)), SMALL, reason)


def _received_order_defects_hold_only_inside_the_support():
    df = _walk(200, 4)
    need = _need(SMALL)
    bad = 120
    flagged = df.copy()
    flagged.attrs = dict(df.attrs)
    flagged.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(flagged, SMALL)
    clean = _apply(df, SMALL)
    assert (out["tsi_reason"].iloc[bad:bad + need] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[:bad], clean[DECISION_COLUMNS].iloc[:bad])
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[bad + need:], clean[DECISION_COLUMNS].iloc[bad + need:])


def test_defects_hold_only_inside_the_support_and_decisions_resume(subtests):
    with subtests.test(case="received_order_defects_hold_only_inside_the_support"):
        _received_order_defects_hold_only_inside_the_support()
    with subtests.test(case="a_defect_holds_only_inside_its_support_and_valid_decisions_resume"):
        _a_defect_holds_only_inside_its_support_and_valid_decisions_resume()


def test_volume_is_not_an_entry_input():
    df = _walk(120, 3)
    a = _apply(df, SMALL)
    b = _apply(_set_last(df, "volume", -1.0), SMALL)
    c = _apply(df.drop(columns=["volume"]), SMALL)
    for other in (b, c):
        pd.testing.assert_frame_equal(a[DECISION_COLUMNS], other[DECISION_COLUMNS])


def _missing_or_wrong_cadence_holds_every_row():
    df = _walk(120, 3)
    df.attrs.pop(FRAME_INTERVAL_ATTR)
    out = _apply(df, SMALL)
    assert (out["tsi_reason"] == "missing_cadence").all() and (out["signal"] == 0).all()
    assert out["tsi_value"].isna().all()
    wrong = _walk(120, 3)
    wrong.attrs[FRAME_INTERVAL_ATTR] = 3_600_000
    out = _apply(wrong, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["tsi_reason"].iloc[_need(SMALL) - 1:]) == {"cadence_gap"}


def test_a_defect_in_the_prior_endpoints_exclusive_oldest_bar_holds_the_decision():
    df = _walk(200, 5)
    p = SMALL
    t = 150
    oldest = t - p["recursion_length"]
    bad = _apply(_set_at(df, oldest, "close", np.nan), p)
    clean = _apply(df, p)
    assert bad["tsi_value_valid"].iloc[t] == 1.0
    assert bad["tsi_value"].iloc[t] == clean["tsi_value"].iloc[t]
    assert bad["tsi_valid"].iloc[t] == 0.0 and int(bad["signal"].iloc[t]) == 0
    assert bad["tsi_reason"].iloc[t] == "nonfinite_input"
    assert np.isnan(bad["tsi_prev_value"].iloc[t])
    assert bad["tsi_valid"].iloc[t + 1] == 1.0
    pd.testing.assert_series_equal(bad[DECISION_COLUMNS].iloc[t + 1], clean[DECISION_COLUMNS].iloc[t + 1],
                                   check_names=False)


def _a_defect_holds_only_inside_its_support_and_valid_decisions_resume():
    df = _walk(300, 5)
    need = _need(SMALL)
    bad = 150
    out = _apply(_set_at(df, bad, "close", np.nan), SMALL)
    clean = _apply(df, SMALL)
    assert (out["tsi_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[:bad], clean[DECISION_COLUMNS].iloc[:bad])
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[bad + need:], clean[DECISION_COLUMNS].iloc[bad + need:])
    gapped = _apply(df.drop(df.index[bad]), SMALL)
    assert (gapped["tsi_reason"].iloc[bad:bad + need - 1] == "cadence_gap").all()
    assert gapped["tsi_valid"].iloc[bad + need - 1] == 1.0


def _overflowing_changes_hold_as_nonfinite_derived():
    closes = [1e300 if i % 2 == 0 else 1.79e308 for i in range(60)]
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _holds_at_end(df, SMALL, "nonfinite_derived")
    assert (out["signal"] == 0).all()
    assert out["tsi_value"].isna().all()


def test_invalid_or_unusable_market_input_holds_with_its_reason(subtests):
    with subtests.test(case="invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame"):
        _invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame()
    with subtests.test(case="missing_or_wrong_cadence_holds_every_row"):
        _missing_or_wrong_cadence_holds_every_row()
    with subtests.test(case="overflowing_changes_hold_as_nonfinite_derived"):
        _overflowing_changes_hold_as_nonfinite_derived()


INVALID_PARAMS = [
    {"slow_period": 1, "recursion_length": 180}, {"fast_period": 1}, {"signal_period": 1}, {"slow_period": 0},
    {"fast_period": 25}, {"fast_period": 26}, {"slow_period": 25.5}, {"slow_period": True}, {"fast_period": False},
    {"slow_period": float("nan")}, {"signal_period": float("inf")}, {"slow_period": "25"}, {"recursion_length": None},
    {"recursion_length": 199}, {"recursion_length": 149}, {"slow_period": 34, "recursion_length": 198},
    {"signal_period": 180}, {"signal_period": 181}, {"recursion_length": 180.5},
    {"slow_period": 2, "fast_period": 1, "recursion_length": 12},
    {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 17},
]


def _unknown_parameters_fail_the_registry_validator():
    reg = _reg()
    with pytest.raises(ValueError):
        reg._registry.validate_params(NAME, {**SEED, "period": 9})


BOUNDARY_PARAMS = [
    SMALL, BOUNDARY, {"slow_period": 33, "fast_period": 32, "signal_period": 197, "recursion_length": 198},
    {"slow_period": 3, "fast_period": 2, "signal_period": 17, "recursion_length": 18},
]


def _boundary_parameter_cases_validate_and_decide_at_w_plus_one_rows():
    for params in BOUNDARY_PARAMS:
        with _case(params):
            _boundary_parameters_validate_and_decide_at_w_plus_one_rows(params)


def _boundary_parameters_validate_and_decide_at_w_plus_one_rows(params):
    reg = _reg()
    reg._registry.validate_params(NAME, params)
    df = _walk(260, 2)
    out = _apply(df, params)
    need = _need(params)
    assert out["tsi_valid"].iloc[need - 2] == 0.0 and out["tsi_reason"].iloc[need - 2] == "insufficient_history"
    assert out["tsi_valid"].iloc[need - 1] == 1.0
    exact = _apply(df.iloc[:need], params)[DECISION_COLUMNS].iloc[-1]
    pd.testing.assert_series_equal(exact, out[DECISION_COLUMNS].iloc[need - 1], check_names=False)
    floats = {k: float(v) for k, v in params.items()}
    pd.testing.assert_frame_equal(_apply(df, floats), out)
    assert _need(BOUNDARY) == 199


def test_parameter_bounds_refuse_unknown_keys_and_accept_every_boundary(subtests):
    with subtests.test(case="unknown_parameters_fail_the_registry_validator"):
        _unknown_parameters_fail_the_registry_validator()
    with subtests.test(case="boundary_parameters_validate_and_decide_at_w_plus_one_rows"):
        _boundary_parameter_cases_validate_and_decide_at_w_plus_one_rows()


def test_walk_forward_warmup_keeps_the_decision_before_the_first_scored_bar(monkeypatch):
    grid = optimizer.generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert strategy_warmup_bars(NAME, grid, DEFAULT_PARAM_RANGES[NAME]) == max(_need(p) for p in grid) == 199
    ranges = {"slow_period": [30], "fast_period": [13], "signal_period": [9], "recursion_length": [180, 198]}
    combos = optimizer.generate_param_grid(ranges)
    warmup = strategy_warmup_bars(NAME, combos, ranges)
    assert warmup == 199
    df = _walk(1500, 5, freq="1h")
    full = {p["recursion_length"]: _apply(df, p)["signal"] for p in combos}
    b = next(i for i in range(warmup, len(df)) if full[180].iloc[i] != 0 and full[198].iloc[i] == full[180].iloc[i])
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
    assert first == {180: int(full[180].iloc[b]), 198: int(full[198].iloc[b])}
    assert 0 not in first.values()


def test_timestamp_column_and_native_close_frames_decide_the_same_way():
    df = _walk(260, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = df.index.as_unit("ms").asi8.astype("int64")
    plain.attrs = dict(df.attrs)
    a = _apply(df, SMALL)
    b = _apply(plain, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
    np.testing.assert_array_equal(a["tsi_value"].to_numpy(), b["tsi_value"].to_numpy())
    native = df.set_axis(df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1))
    _declare(native, "4h", "native_close_inclusive")
    c = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), c["signal"].to_numpy())
    np.testing.assert_array_equal(a["tsi_eval_ts_ms"].to_numpy(), c["tsi_eval_ts_ms"].to_numpy())
    assert a["tsi_eval_ts_ms"].iloc[-1] == df.index[-1].value // 1_000_000
    native.attrs.pop(FRAME_TIMESTAMP_KIND_ATTR)
    d = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), d["signal"].to_numpy())
    assert d["tsi_eval_ts_ms"].isna().all()


def test_full_series_every_prefix_exact_support_and_200_row_frames_agree_on_frozen_candles():
    df = _frozen("BTC", "1h", 460)
    for p in (SEED, BOUNDARY, {"slow_period": 30, "fast_period": 8, "signal_period": 9, "recursion_length": 198}):
        need = _need(p)
        full = _apply(df, p)
        assert full["tsi_valid"].iloc[need - 1:].all()
        for end in range(need, len(df) + 1):
            prefix = _apply(df.iloc[:end], p)[DECISION_COLUMNS].iloc[-1]
            pd.testing.assert_series_equal(prefix, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            if end >= 200:
                tail = _apply(df.iloc[end - 200:end], p)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(tail, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            span = _apply(df.iloc[end - need:end], p)[DECISION_COLUMNS].iloc[-1]
            pd.testing.assert_series_equal(span, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
        short = _apply(df.iloc[len(df) - need + 1:], p)
        assert short["tsi_reason"].iloc[-1] == "insufficient_history"


def _every_grid_combination_is_causal_on_frozen_candles():
    df = _frozen("ETH", "4h", 420)
    future = df.copy()
    future.iloc[300:, future.columns.get_loc("close")] *= 1.3
    future.iloc[300:, future.columns.get_loc("high")] *= 1.3
    for _, p in expand_sweep(SEED, [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]):
        full = _apply(df, p)
        cut = _apply(df.iloc[:300], p)
        changed = _apply(future, p)
        np.testing.assert_array_equal(full["signal"].iloc[:300].to_numpy(), cut["signal"].to_numpy())
        np.testing.assert_array_equal(full["signal"].iloc[:300].to_numpy(), changed["signal"].iloc[:300].to_numpy())


def test_frozen_candles_produce_both_directions_with_the_seed():
    out = _apply(_frozen("SOL", "4h", 1500), SEED)
    assert {-1, 1} <= set(out["signal"].unique())


def _rows_outside_the_support_cannot_change_a_decision():
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
    assert recovered["tsi_valid"].iloc[t - 1] == 0.0


def test_rows_outside_the_support_and_later_rows_never_change_a_decision(subtests):
    with subtests.test(case="every_grid_combination_is_causal_on_frozen_candles"):
        _every_grid_combination_is_causal_on_frozen_candles()
    with subtests.test(case="rows_outside_the_support_cannot_change_a_decision"):
        _rows_outside_the_support_cannot_change_a_decision()


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
proto["arms"] = {a: proto["arms"][a] for a in proto["verdict_rules"]["required_arms"]}
runner = rs.Runner(proto)
reg = runner.reg.STRATEGY_REGISTRY
sel = {a: ({k: proto["seed_params"][k] for k in proto["seed_params"]} if a == proto["candidate_arm"] else
           {k: reg[s["name"]]["default_params"][k] for k in proto["grids"][s["name"]]})
       for a, s in proto["arms"].items()}
g = {"selections": {a: {"how": "selected", "params": p} for a, p in sel.items()}}
g["held_out"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe")} for a in proto["arms"]}
g["held_out_x2_slippage"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe", slippage_mult=2.0)}
                             for a in proto["stress_arms"]}
out = {"verdict": rs.group_verdict(proto, g, True, True),
       "legs": {a: g["held_out"][a]["probe"]["legs"] for a in proto["arms"]}}
print(json.dumps(out, default=str))
"""


def _cache_case(tmp_path, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {})],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _missing_held_hour_funding_makes_every_required_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
    lo = int(pd.Timestamp("2025-03-01").timestamp() * 1000)
    hi = int(pd.Timestamp("2025-04-01").timestamp() * 1000)
    out = _cache_case(tmp_path, {"BTC": [lo, hi]})
    verdict = out["verdict"]
    assert verdict["verdict"] == "inconclusive"
    assert verdict["gates"]["all_windows_scorable"] is False
    bad = verdict["checks"]["unscorable_windows"]["probe"]
    for label in ("candidate BTC/USDT 1h", "candidate x2 slippage BTC/USDT 1h", "momentum_pro BTC/USDT 1h",
                  "vol_momentum BTC/USDT 1h", "trix_momentum BTC/USDT 1h", "incumbent median BTC/USDT 1h"):
        assert label in bad, bad
    funding = out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]
    assert funding["unpriced_held_hours"] > 0 and funding["mode"] == "charge"
    assert 0 <= funding["priced_hours"] < funding["expected_hours"]
    assert funding["coverage_complete"] is False


def _complete_funding_gives_scorable_bidirectional_legs_with_next_open_fills(tmp_path):
    out = _cache_case(tmp_path)
    verdict = out["verdict"]
    assert verdict["gates"]["all_windows_scorable"] is True
    for arm, legs in out["legs"].items():
        leg = legs["BTC/USDT 1h"]
        assert leg["funding"]["mode"] == "charge" and leg["funding_incomplete"] is False
        assert leg["warmup"]["bars"] == 300
        assert leg["next_bar_fills"]["mismatches"] == 0
    assert verdict["checks"]["long_positions"]["candidate"] > 0
    assert verdict["checks"]["short_positions"]["candidate"] > 0


FUNDING_CASES = [
    ("missing_held_hour_funding_is_inconclusive",
     _missing_held_hour_funding_makes_every_required_arm_unscorable_and_the_verdict_inconclusive),
    ("complete_funding_is_scorable_with_next_open_fills",
     _complete_funding_gives_scorable_bidirectional_legs_with_next_open_fills),
]


def test_study_funding_coverage_decides_whether_every_leg_is_scorable(tmp_path, subtests):
    for label, run in FUNDING_CASES:
        with subtests.test(case=label):
            case_dir = tmp_path / label
            case_dir.mkdir()
            run(case_dir)
