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
from closed_bar import FRAME_INTERVAL_ATTR, FRAME_TIMESTAMP_KIND_ATTR, declare_frame_cadence
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

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


def _close_params():
    return _close_refs()[0]["params"]


def _reg():
    return load_registry("futures")


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


def test_registration_metadata_grid_and_hidden_discovery():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(os.path.join(STUDY, "protocol.json")) as fh:
        proto = json.load(fh)
    assert entry["default_params"] == SEED == proto["seed_params"]
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/trix_momentum_1659/REPORT.md"
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == proto["grids"][NAME]
    combos = [p for _, p in expand_sweep(SEED, [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])]
    assert len(combos) == 18
    for p in combos:
        fut._registry.validate_params(NAME, p)
    assert max(_need(p) for p in combos) == 194
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


def test_crossing_boundaries_and_the_zero_line_filter():
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


def test_sustained_conditions_do_not_repeat_entries():
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


@pytest.mark.parametrize("mutate,reason", [
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
])
def test_invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame(mutate, reason):
    _holds_at_end(mutate(_walk(120, 3)), SMALL, reason)


def test_negative_or_missing_volume_is_not_an_entry_input():
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
    assert (out["trix_reason"] == "missing_cadence").all() and (out["signal"] == 0).all()
    assert out["trix_value"].isna().all()
    wrong = _walk(120, 3)
    wrong.attrs[FRAME_INTERVAL_ATTR] = 3_600_000
    out = _apply(wrong, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["trix_reason"].iloc[_need(SMALL) - 1:]) == {"cadence_gap"}


def test_a_defect_in_the_prior_endpoints_exclusive_oldest_bar_holds_the_decision():
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


def test_overflowing_prices_hold_as_nonfinite_derived():
    closes = [1e308 if i % 2 == 0 else 1.7e308 for i in range(60)]
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _holds_at_end(df, SMALL, "nonfinite_derived")
    assert (out["signal"] == 0).all()
    assert out["trix_value"].isna().all()


def test_subnormal_prices_hold_as_invalid_denominator():
    closes = [5e-324] * 60
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _apply(df, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["trix_reason"].iloc[_need(SMALL) - 1:]) <= {"invalid_denominator", "nonfinite_derived", "no_cross"}


def test_constant_prices_never_enter_and_report_finite_zero_values():
    out = _apply(_from_closes([250.0] * 200), SEED)
    assert (out["signal"] == 0).all()
    valid = out[out["trix_valid"] == 1.0]
    assert len(valid) == 200 - _need(SEED) + 1
    assert (valid["trix_reason"] == "no_cross").all()
    assert (valid["trix_value"] == 0.0).all() and (valid["trix_signal_line"] == 0.0).all()


def test_rising_and_falling_series_report_the_matching_oscillator_sign():
    up = _apply(_from_closes([100.0 * 1.002 ** i for i in range(200)]), SEED)
    down = _apply(_from_closes([100.0 * 0.998 ** i for i in range(200)]), SEED)
    assert (up["trix_value"].dropna() > 0).all() and (down["trix_value"].dropna() < 0).all()
    assert (up["signal"] == 0).all() and (down["signal"] == 0).all()


def test_a_defect_holds_only_inside_its_dependency_window():
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


@pytest.mark.parametrize("params", [
    {"period": 0}, {"period": -1}, {"period": 9.5}, {"period": True}, {"period": float("nan")}, {"period": "18"},
    {"period": 33, "recursion_length": 197}, {"period": 21}, {"signal_period": 0}, {"signal_period": False},
    {"signal_period": 120}, {"signal_period": 121}, {"signal_period": float("inf")}, {"recursion_length": 198},
    {"recursion_length": 107}, {"recursion_length": 120.5}, {"recursion_length": None},
    {"period": 1, "recursion_length": 5, "signal_period": 1},
])
def test_invalid_parameters_fail_validation(params):
    p = {**SEED, **params}
    reg = _reg()
    with pytest.raises(ValueError):
        reg._registry.validate_params(NAME, p)
    with pytest.raises(ValueError):
        _apply(_walk(120, 1), p)
    from trix_momentum import trix_momentum_core
    with pytest.raises(ValueError):
        trix_momentum_core(_walk(120, 1), **p)


@pytest.mark.parametrize("params", [SMALLEST, BOUNDARY, {"period": 32, "signal_period": 191, "recursion_length": 192},
                                    {"period": 1, "signal_period": 5, "recursion_length": 6}])
def test_boundary_parameters_validate_and_decide_at_l_plus_two_rows(params):
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


def test_full_series_every_prefix_and_every_200_row_frame_agree_on_frozen_candles():
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


def test_frozen_candles_produce_both_directions_with_the_seed():
    out = _apply(_frozen("SOL", "4h", 1500), SEED)
    assert {-1, 1} <= set(out["signal"].unique())


def test_rows_outside_the_dependency_window_cannot_change_a_decision():
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


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": _close_params()})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    base = [100.0] * 20
    long_df = _from_closes(base + [101.0])
    short_df = _from_closes(base + [99.0])
    _, d = _composer(long_df, TINY)
    assert (d["open_action"], d["signal"]) == ("long", 1)
    _, d = _composer(short_df, TINY)
    assert (d["open_action"], d["signal"]) == ("short", -1)
    for df in (long_df, short_df):
        mark = float(df["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
            _, held = _composer(df, TINY, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("mutation", ["price", "geometry", "cadence", "insufficient", "order"])
def test_entry_data_hold_keeps_the_close_owner_executable(mutation):
    df = _walk(200, 9)
    if mutation == "price":
        df = _set_at(df, -5, "close", np.nan)
    elif mutation == "geometry":
        df = _set_last(df, "open", df["high"].iloc[-1] * 2)
    elif mutation == "cadence":
        df = _with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)])
    elif mutation == "order":
        df = _with_index(df, list(df.index[:-1]) + [df.index[-3]])
    else:
        df = df.iloc[-(_need(SMALL) - 1):]
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, SMALL, "long", ctx)
    assert evaluation.open_result_df["trix_valid"].iloc[-1] == 0.0
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


def test_simulator_long_only_never_opens_a_short():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, SMALL))
    assert (sig["signal"] == -1).any()
    res = _bt(SMALL, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


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


@pytest.mark.parametrize("window", [200, None])
@pytest.mark.parametrize("params", [SEED, BOUNDARY])
def test_raw_entry_parity_on_frozen_candles(window, params):
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


def _cache_case(tmp_path, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {})],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def test_missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
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


def test_complete_funding_gives_scorable_legs_and_long_only_comparators(tmp_path):
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
