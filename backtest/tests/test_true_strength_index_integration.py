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
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
import optimizer
from optimizer import DEFAULT_PARAM_RANGES, strategy_warmup_bars
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "true_strength_index"
REF = "backtest/candidates/true_strength_index_1667/REPORT.md"
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


def _close_params():
    return _close_refs()[0]["params"]


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


def test_registration_metadata_grid_and_hidden_discovery():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(os.path.join(STUDY, "protocol.json")) as fh:
        proto = json.load(fh)
    assert entry["default_params"] == SEED == proto["seed_params"]
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == REF
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert fut._registry.PLATFORM_ORDER["futures"].count(NAME) == 1
    assert NAME not in fut._registry.PLATFORM_ORDER["spot"]
    assert DEFAULT_PARAM_RANGES[NAME] == proto["grids"][NAME]
    combos = [p for _, p in expand_sweep(SEED, [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])]
    assert len(combos) == 54
    for p in combos:
        fut._registry.validate_params(NAME, p)
    assert max(_need(p) for p in combos) == 199
    for platform in ("spot", "futures"):
        out = subprocess.run([sys.executable, os.path.join(_REPO, "shared_strategies", "open", platform,
                                                          "strategies.py"), "--list-json"],
                             capture_output=True, text=True, cwd=_REPO, check=True).stdout
        captures = []
        for phase in ("before", "after"):
            with open(os.path.join(STUDY, f"discovery_{phase}_{platform}.json"), "rb") as fh:
                captures.append(fh.read())
        assert captures[0] == captures[1]
        assert NAME.encode() not in captures[1]
        assert NAME not in out


@pytest.mark.parametrize("params", [SMALL, dict(SMALL, signal_period=5), dict(SMALL, slow_period=4, fast_period=3,
                                                                            recursion_length=24)])
def test_values_match_an_independent_scalar_reference_and_both_entry_directions(params):
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


def test_invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_MARKET_INPUTS):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(120, 3)), SMALL, reason)


def test_received_order_defects_hold_only_inside_the_support():
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


def test_volume_is_not_an_entry_input():
    df = _walk(120, 3)
    a = _apply(df, SMALL)
    b = _apply(_set_last(df, "volume", -1.0), SMALL)
    c = _apply(df.drop(columns=["volume"]), SMALL)
    for other in (b, c):
        pd.testing.assert_frame_equal(a[DECISION_COLUMNS], other[DECISION_COLUMNS])


def test_missing_or_wrong_cadence_holds_every_row():
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


def test_a_defect_holds_only_inside_its_support_and_valid_decisions_resume():
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


def test_overflowing_changes_hold_as_nonfinite_derived():
    closes = [1e300 if i % 2 == 0 else 1.79e308 for i in range(60)]
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _holds_at_end(df, SMALL, "nonfinite_derived")
    assert (out["signal"] == 0).all()
    assert out["tsi_value"].isna().all()


INVALID_PARAMS = [
    {"slow_period": 1, "recursion_length": 180}, {"fast_period": 1}, {"signal_period": 1}, {"slow_period": 0},
    {"fast_period": 25}, {"fast_period": 26}, {"slow_period": 25.5}, {"slow_period": True}, {"fast_period": False},
    {"slow_period": float("nan")}, {"signal_period": float("inf")}, {"slow_period": "25"}, {"recursion_length": None},
    {"recursion_length": 199}, {"recursion_length": 149}, {"slow_period": 34, "recursion_length": 198},
    {"signal_period": 180}, {"signal_period": 181}, {"recursion_length": 180.5},
    {"slow_period": 2, "fast_period": 1, "recursion_length": 12},
    {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 17},
]


def test_invalid_parameters_fail_validation():
    reg = _reg()
    for params in INVALID_PARAMS:
        p = {**SEED, **params}
        with _case(params):
            with pytest.raises(ValueError):
                reg._registry.validate_params(NAME, p)
            with pytest.raises(ValueError):
                _apply(_walk(120, 1), p)
            from true_strength_index import true_strength_index_core
            with pytest.raises(ValueError):
                true_strength_index_core(_walk(120, 1), **p)


def test_unknown_parameters_fail_the_registry_validator():
    reg = _reg()
    with pytest.raises(ValueError):
        reg._registry.validate_params(NAME, {**SEED, "period": 9})


BOUNDARY_PARAMS = [
    SMALL, BOUNDARY, {"slow_period": 33, "fast_period": 32, "signal_period": 197, "recursion_length": 198},
    {"slow_period": 3, "fast_period": 2, "signal_period": 17, "recursion_length": 18},
]


def test_boundary_parameters_validate_and_decide_at_w_plus_one_rows():
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


def test_every_grid_combination_is_causal_on_frozen_candles():
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


def test_rows_outside_the_support_cannot_change_a_decision():
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


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": _close_params()})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    flat = [100.0] * 20
    long_df = _from_closes(flat + [99.0, 104.0])
    short_df = _from_closes(flat + [101.0, 96.0])
    _, d = _composer(long_df, SMALL)
    assert (d["open_action"], d["signal"]) == ("long", 1)
    _, d = _composer(short_df, SMALL)
    assert (d["open_action"], d["signal"]) == ("short", -1)
    for df in (long_df, short_df):
        mark = float(df["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
            _, held = _composer(df, SMALL, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


def test_entry_data_hold_keeps_the_close_owner_executable():
    for mutation in ("price", "geometry", "cadence", "insufficient", "order", "denominator"):
        with _case(mutation):
            _entry_data_hold_keeps_the_close_owner_executable(mutation)


def _entry_data_hold_keeps_the_close_owner_executable(mutation):
    df = _walk(200, 9)
    if mutation == "price":
        df = _set_at(df, -5, "close", np.nan)
    elif mutation == "geometry":
        df = _set_last(df, "open", df["high"].iloc[-1] * 2)
    elif mutation == "cadence":
        df = _with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)])
    elif mutation == "order":
        df = _with_index(df, list(df.index[:-1]) + [df.index[-3]])
    elif mutation == "denominator":
        df = _from_closes([200.0] * 60)
    else:
        df = df.iloc[-(_need(SMALL) - 1):]
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, SMALL, "long", ctx)
    assert evaluation.open_result_df["tsi_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, decision = _composer(df, SMALL, "short", dict(ctx, side="short", avg_cost=mark * 1.05))
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    _, decision = _composer(df, SMALL, "long", dict(ctx, avg_cost=mark / 1.015))
    assert decision["close_fraction"] == 0.25


def test_close_owner_passes_strict_capability_validation():
    report = validate_close_capabilities(close_refs=_close_refs(), comparison_mode=None, platform="hyperliquid",
                                         strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=_close_refs(), stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, SMALL))
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


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    df = _walk(800, 5)
    sig = ensure_atr_indicator(_apply(df, SMALL))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 100 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    later = corrupt.index[first + 1:]
    shift = later + pd.to_timedelta(np.arange(1, len(later) + 1), unit="min")
    corrupt = _with_index(corrupt, list(corrupt.index[:first + 1]) + list(shift))
    bad = ensure_atr_indicator(_apply(corrupt, SMALL))
    assert (bad["signal"].iloc[:first + 1].to_numpy() == sig["signal"].iloc[:first + 1].to_numpy()).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    res = _bt(SMALL).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    last_entry = max(pd.Timestamp(t["entry_date"]) for t in res["trades"])
    assert last_entry == bad.index[first + 1]
    final = [t for t in res["trades"] if pd.Timestamp(t["entry_date"]) == last_entry]
    assert {t["exit_reason"].split(":")[0] for t in final} <= {"sl", "tiered_tp_pct"}


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [200, 181, None])
@pytest.mark.parametrize("params", [SEED, BOUNDARY])
def test_raw_entry_parity_on_frozen_candles(window, params):
    if window == 181:
        window = _need(params)
    frame = compute_parity_frame(_frozen("BTC", "4h", 420), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_flat_long_and_short_contexts(window):
    cfg = _cfg(SMALL, close_refs=_close_refs(), direction="both")
    frame = compute_parity_frame(_frozen("SOL", "4h", 620), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    cfg = _cfg(SMALL, close_refs=_close_refs(), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC", "4h", 320), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()


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


def test_missing_held_hour_funding_makes_every_required_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
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


def test_complete_funding_gives_scorable_bidirectional_legs_with_next_open_fills(tmp_path):
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
