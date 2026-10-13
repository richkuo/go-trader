import json
import os
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "stochastic_momentum_index"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663", "candidate_seed.json")
TIERS = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
CLOSE = [{"name": "tiered_tp_pct", "params": TIERS}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
FOUR_H = 14_400_000
DEFAULTS = {"range_period": 10, "smoothing_period": 3, "signal_period": 3, "recursion_length": 60,
            "oversold": -40.0, "overbought": 40.0}
NEED = 71
DOMAIN_ENDPOINTS = [
    dict(DEFAULTS, range_period=1, smoothing_period=1, signal_period=1, recursion_length=8, oversold=-99.5,
         overbought=99.5),
    dict(DEFAULTS, range_period=190, smoothing_period=1, signal_period=1, recursion_length=8),
    dict(DEFAULTS, range_period=6, smoothing_period=24, signal_period=24, recursion_length=192),
    dict(DEFAULTS, range_period=14, smoothing_period=4, signal_period=2, recursion_length=32),
]


def _reg():
    return load_registry("futures")


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(params):
    return int(params["recursion_length"]) + int(params["range_period"]) + 1


def _walk(n, seed, start=100.0, sigma=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, sigma, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return declare_frame_cadence(df, "4h", TIMESTAMP_KIND_OPEN)


def ref_endpoint(high, low, close, t, params):
    k, length = int(params["range_period"]), int(params["recursion_length"])
    a = 2.0 / (int(params["smoothing_period"]) + 1)
    b = 2.0 / (int(params["signal_period"]) + 1)
    keep_a, keep_b = 1.0 - a, 1.0 - b
    rel, rng = [], []
    for i in range(t - length, t + 1):
        hh = max(float(x) for x in high[i - k + 1:i + 1])
        ll = min(float(x) for x in low[i - k + 1:i + 1])
        rel.append(float(close[i]) - (hh + ll) / 2.0)
        rng.append(hh - ll)
    n1 = n2 = rel[0]
    d1 = d2 = rng[0]
    zero = not d2 > 0
    osc = 200.0 * n2 / d2 if d2 > 0 else float("nan")
    line = osc
    for j in range(1, length + 1):
        n1 = a * rel[j] + keep_a * n1
        n2 = a * n1 + keep_a * n2
        d1 = a * rng[j] + keep_a * d1
        d2 = a * d1 + keep_a * d2
        if not d2 > 0:
            zero = True
            osc = float("nan")
        else:
            osc = 200.0 * n2 / d2
        line = b * osc + keep_b * line
    return {"value": osc, "line": line, "diff": osc - line, "rel": n2, "range": d2, "seed_rel": rel[0],
            "seed_range": rng[0], "zero": zero}


def ref_decision(cur, prev, params):
    if cur["zero"] or prev["zero"]:
        return None
    lo, hi = params["oversold"], params["overbought"]
    if prev["value"] <= lo < cur["value"] and prev["diff"] <= 0.0 < cur["diff"]:
        return 1
    if prev["value"] >= hi > cur["value"] and prev["diff"] >= 0.0 > cur["diff"]:
        return -1
    return 0


def test_registration_is_futures_only_bidirectional_no_edge_hidden_and_seeded():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(SEED) as fh:
        seed = json.load(fh)
    assert entry["default_params"] == seed["params"] == DEFAULTS
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/stochastic_momentum_index_1663/REPORT.md"
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == seed["optimizer_grid"]
    combos = expand_sweep(dict(DEFAULTS), [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])
    assert len(combos) == 729
    for _, params in combos:
        fut._registry.validate_params(NAME, params)


@pytest.mark.parametrize("params", [DEFAULTS, dict(DEFAULTS, range_period=14, smoothing_period=4, signal_period=2,
                                                   recursion_length=80, oversold=-30.0, overbought=50.0)]
                         + DOMAIN_ENDPOINTS)
def test_values_and_decisions_match_an_independent_scalar_replay(params):
    df = _walk(420, 7)
    out = _apply(df, params)
    high, low, close = (df[c].to_numpy() for c in ("high", "low", "close"))
    need = _need(params)
    assert (out["smi_reason"].iloc[:need - 1] == "insufficient_history").all()
    assert (out["smi_required_history"] == float(need)).all()
    seen = set()
    for t in range(need - 2, len(df)):
        cur = ref_endpoint(high, low, close, t, params)
        row = out.iloc[t]
        assert row["smi_value_valid"] == (0.0 if cur["zero"] else 1.0)
        if not cur["zero"]:
            assert row["smi_value"] == cur["value"], t
            assert row["smi_signal_line"] == cur["line"], t
            assert row["smi_diff"] == cur["diff"], t
            assert row["smi_relative_smoothed"] == cur["rel"] and row["smi_range_smoothed"] == cur["range"]
            assert row["smi_seed_relative"] == cur["seed_rel"] and row["smi_seed_range"] == cur["seed_range"]
        if t < need - 1:
            continue
        prev = ref_endpoint(high, low, close, t - 1, params)
        want = ref_decision(cur, prev, params)
        if want is None:
            assert int(row["signal"]) == 0 and row["smi_valid"] == 0.0 and row["smi_reason"] == "zero_range"
            continue
        assert row["smi_valid"] == 1.0
        assert row["smi_prev_value"] == prev["value"] and row["smi_prev_signal_line"] == prev["line"]
        assert row["smi_prev_diff"] == prev["diff"]
        assert row["smi_prev_seed_relative"] == prev["seed_rel"] and row["smi_prev_seed_range"] == prev["seed_range"]
        assert int(row["signal"]) == want, t
        seen.add(want)
    if params is DEFAULTS:
        assert seen == {-1, 0, 1}


def test_previous_endpoint_is_replayed_from_its_own_seed():
    df = _walk(420, 7)
    out = _apply(df, DEFAULTS)
    high, low, close = (df[c].to_numpy() for c in ("high", "low", "close"))
    a = 2.0 / 4.0
    b = 2.0 / 4.0
    differs = 0
    for t in range(NEED - 1, len(df)):
        rel, rng = [], []
        for i in range(t - 60, t + 1):
            hh, ll = float(np.max(high[i - 9:i + 1])), float(np.min(low[i - 9:i + 1]))
            rel.append(float(close[i]) - (hh + ll) / 2.0)
            rng.append(hh - ll)
        n1 = n2 = rel[0]
        d1 = d2 = rng[0]
        line = 200.0 * n2 / d2
        osc = line
        series = []
        for j in range(1, 61):
            n1 = a * rel[j] + (1 - a) * n1
            n2 = a * n1 + (1 - a) * n2
            d1 = a * rng[j] + (1 - a) * d1
            d2 = a * d1 + (1 - a) * d2
            osc = 200.0 * n2 / d2
            line = b * osc + (1 - b) * line
            series.append((osc, line))
        penultimate = series[-2]
        row = out.iloc[t]
        assert row["smi_value"] == osc and row["smi_signal_line"] == line
        own = ref_endpoint(high, low, close, t - 1, DEFAULTS)
        assert row["smi_prev_value"] == own["value"] and row["smi_prev_signal_line"] == own["line"]
        if (penultimate[0], penultimate[1]) != (own["value"], own["line"]):
            differs += 1
    assert differs > 0


def _first(out, side=None, reason=None):
    if reason is not None:
        rows = np.flatnonzero(out["smi_reason"].to_numpy() == reason)
    else:
        rows = np.flatnonzero(out["signal"].to_numpy() == side)
    assert len(rows)
    return int(rows[0])


@pytest.mark.parametrize("side", [1, -1])
def test_threshold_equality_and_adjacent_floats_follow_the_strict_rules(side):
    df = _walk(900, 11)
    out = _apply(df, DEFAULTS)
    i = _first(out, side)
    prev, cur = float(out["smi_prev_value"].iloc[i]), float(out["smi_value"].iloc[i])
    key = "oversold" if side == 1 else "overbought"
    other = "overbought" if side == 1 else "oversold"
    keep = {other: 99.0 if side == 1 else -99.0}
    at_prev = _apply(df, dict(DEFAULTS, **keep, **{key: prev}))
    assert int(at_prev["signal"].iloc[i]) == side
    at_cur = _apply(df, dict(DEFAULTS, **keep, **{key: cur}))
    assert int(at_cur["signal"].iloc[i]) == 0 and at_cur["smi_reason"].iloc[i] == "signal_cross_only"
    toward = float(np.nextafter(prev, -np.inf if side == 1 else np.inf))
    past = _apply(df, dict(DEFAULTS, **keep, **{key: toward}))
    assert int(past["signal"].iloc[i]) == 0 and past["smi_reason"].iloc[i] == "signal_cross_only"
    inside = float(np.nextafter(cur, -np.inf if side == 1 else np.inf))
    still = _apply(df, dict(DEFAULTS, **keep, **{key: inside}))
    assert int(still["signal"].iloc[i]) == side


def test_single_crossings_hold_and_sustained_conditions_do_not_repeat():
    df = _walk(3000, 1)
    out = _apply(df, DEFAULTS)
    level = out.iloc[_first(out, reason="level_cross_only")]
    up = level["smi_prev_value"] <= -40.0 < level["smi_value"]
    if up:
        assert not (level["smi_prev_diff"] <= 0.0 < level["smi_diff"])
    else:
        assert level["smi_prev_value"] >= 40.0 > level["smi_value"]
        assert not (level["smi_prev_diff"] >= 0.0 > level["smi_diff"])
    signal_only = out.iloc[_first(out, reason="signal_cross_only")]
    assert int(signal_only["signal"]) == 0
    signal = out["signal"].to_numpy()
    entries = np.flatnonzero(signal != 0)
    assert len(entries) > 20
    for a in entries:
        if a + 1 < len(signal):
            assert signal[a + 1] != signal[a]
    above = (out["smi_value"].to_numpy() > -40.0) & (out["smi_diff"].to_numpy() > 0.0)
    sustained = np.flatnonzero(above[1:] & above[:-1]) + 1
    assert len(sustained) and not (signal[sustained] == 1).any()


def _holds_at_end(df, params, reason):
    before = df.copy()
    attrs = dict(df.attrs)
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    assert df.attrs == attrs
    pd.testing.assert_index_equal(out.index, before.index)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["smi_valid"].iloc[-1] == 0.0
    assert out["smi_reason"].iloc[-1] == reason, out["smi_reason"].iloc[-1]
    return out


def _with_index(df, idx):
    out = df.set_axis(pd.DatetimeIndex(idx))
    out.attrs = dict(df.attrs)
    return out


def _shift_index(df, pos, delta):
    idx = list(df.index)
    idx[pos] = idx[pos] + delta
    return _with_index(df, idx)


def _no_cadence(df):
    out = df.copy()
    out.attrs = {}
    return out


def _set_last(df, col, value):
    return df.assign(**{col: np.r_[df[col].to_numpy()[:-1], value]})


@pytest.mark.parametrize("mutate,reason", [
    (lambda df: df.drop(columns=["low"]), "missing_columns"),
    (_no_cadence, "missing_cadence"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [pd.NaT]), "missing_timestamps"),
    (lambda df: _set_last(df, "high", np.nan), "nonfinite_input"),
    (lambda df: _set_last(df, "close", np.inf), "nonfinite_input"),
    (lambda df: df.assign(open=["x"] * (len(df) - 1) + [df["open"].iloc[-1]]), "nonfinite_input"),
    (lambda df: _set_last(df, "low", 0.0), "nonpositive_price"),
    (lambda df: _set_last(df, "close", -1.0), "nonpositive_price"),
    (lambda df: _set_last(df, "open", df["high"].iloc[-1] * 2), "invalid_candle_geometry"),
    (lambda df: _set_last(df, "high", df["low"].iloc[-1] / 2), "invalid_candle_geometry"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-2]) + [df.index[-1], df.index[-2]]), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "cadence_gap"),
    (lambda df: _shift_index(df, len(df) - NEED + 2, pd.Timedelta(hours=1)), "cadence_gap"),
    (lambda df: df.iloc[-(NEED - 1):], "insufficient_history"),
])
def test_missing_and_malformed_inputs_are_reasoned_holds_that_preserve_the_frame(mutate, reason):
    _holds_at_end(mutate(_walk(300, 3)), DEFAULTS, reason)


def test_volume_is_not_an_input():
    base = _walk(400, 3)
    a = _apply(base, DEFAULTS)
    for variant in (base.drop(columns=["volume"]), base.assign(volume=np.nan), base.assign(volume=-1.0)):
        b = _apply(variant, DEFAULTS)
        np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
        assert (a["smi_reason"] == b["smi_reason"]).all()


def test_constant_prices_hold_on_zero_range_and_flat_ranges_give_no_cross():
    flat = _walk(300, 3).assign(open=100.0, high=100.0, low=100.0, close=100.0)
    out = _holds_at_end(flat, DEFAULTS, "zero_range")
    assert (out["smi_value_valid"] == 0.0).all() and (out["signal"] == 0).all()
    banded = _walk(300, 3).assign(open=100.0, high=101.0, low=99.0, close=100.0)
    out = _apply(banded, DEFAULTS)
    assert out["smi_reason"].iloc[-1] == "no_cross" and out["smi_value"].iloc[-1] == 0.0


def test_a_zero_range_seed_bar_holds_only_the_decisions_it_seeds():
    df = _walk(500, 5)
    seed_bar = 250
    flat = df.copy()
    rows = slice(seed_bar - 9, seed_bar + 1)
    level = float(flat["close"].iloc[seed_bar - 10])
    for col in ("open", "high", "low", "close"):
        flat.iloc[rows, flat.columns.get_loc(col)] = level
    out = _apply(flat, DEFAULTS)
    t = seed_bar + 60
    assert out["smi_value_valid"].iloc[t] == 0.0
    assert out["smi_reason"].iloc[t] == "zero_range" and out["smi_reason"].iloc[t + 1] == "zero_range"
    assert out["smi_prev_valid"].iloc[t + 1] == 0.0
    assert out["smi_value_valid"].iloc[t - 1] == 1.0
    later = t + 2
    while out["smi_value_valid"].iloc[later] == 0.0:
        later += 1
    assert out["smi_seed_range"].iloc[later] > 0


def test_received_out_of_order_rows_hold_only_inside_their_span():
    df = _walk(500, 5)
    clean = _apply(df, DEFAULTS)
    marked = df.copy()
    bad = 250
    marked.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(marked, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + NEED] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])


def test_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(500, 5)
    bad = 250
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("high")] = np.nan
    out = _apply(corrupt, DEFAULTS)
    clean = _apply(df, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED] == "nonfinite_input").all()
    assert (out["smi_valid"].iloc[bad:bad + NEED] == 0.0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED - 1] == "cadence_gap").all()
    assert out["smi_reason"].iloc[bad + NEED - 1] != "cadence_gap"


@pytest.mark.parametrize("params", [
    {"range_period": 0}, {"range_period": -3}, {"range_period": 10.5}, {"range_period": True},
    {"range_period": "10"}, {"range_period": float("nan")}, {"smoothing_period": 0}, {"smoothing_period": 2.5},
    {"smoothing_period": False}, {"signal_period": 0}, {"signal_period": float("inf")}, {"recursion_length": 0},
    {"recursion_length": 23}, {"smoothing_period": 8}, {"signal_period": 8},
    {"recursion_length": 189, "range_period": 10}, {"recursion_length": 60, "range_period": 139},
    {"oversold": -100.0}, {"overbought": 100.0}, {"oversold": 40.0, "overbought": 40.0},
    {"oversold": 50.0, "overbought": 40.0}, {"oversold": float("nan")}, {"overbought": float("inf")},
    {"oversold": "-40"}, {"overbought": True},
])
def test_invalid_parameters_are_explicit_errors(params):
    with pytest.raises(ValueError):
        _apply(_walk(300, 1), dict(DEFAULTS, **params))
    with pytest.raises(ValueError):
        _reg()._registry.validate_params(NAME, dict(DEFAULTS, **params))


@pytest.mark.parametrize("params", [
    dict(DEFAULTS, recursion_length=24),
    dict(DEFAULTS, recursion_length=188, range_period=10),
    dict(DEFAULTS, range_period=190, smoothing_period=1, signal_period=1, recursion_length=8),
    dict(DEFAULTS, range_period=10.0, smoothing_period=3.0, signal_period=3.0, recursion_length=60.0),
])
def test_boundary_parameters_are_accepted(params):
    _reg()._registry.validate_params(NAME, params)
    out = _apply(_walk(300, 1), params)
    assert out["smi_required_history"].iloc[-1] == float(_need(params))


def test_integral_float_periods_match_integers():
    df = _walk(300, 1)
    floats = dict(DEFAULTS, range_period=10.0, smoothing_period=3.0, signal_period=3.0, recursion_length=60.0)
    pd.testing.assert_frame_equal(_apply(df, floats), _apply(df, DEFAULTS))


@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [DEFAULTS])
def test_dependency_span_is_required_and_sufficient(params):
    df = _walk(420, 9)
    full = _apply(df, params)
    need = _need(params)
    for end in range(need, 420, 29):
        exact = _apply(df.iloc[end - need:end], params)
        assert exact["smi_reason"].iloc[-1] != "insufficient_history"
        assert exact["smi_reason"].iloc[-1] == full["smi_reason"].iloc[end - 1]
        short = _apply(df.iloc[end - need + 1:end], params)
        assert short["smi_reason"].iloc[-1] == "insufficient_history"


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, "4h", TIMESTAMP_KIND_OPEN)


DECISION_COLUMNS = ["smi_value", "smi_prev_value", "smi_signal_line", "smi_prev_signal_line", "smi_diff",
                    "smi_prev_diff", "smi_valid", "smi_reason_code", "signal"]


def test_full_series_prefix_exact_span_and_200_row_tail_agree_for_the_grid_and_bounds():
    df = _frozen("ETH 4h", 700)
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    grid = [p for _, p in expand_sweep(dict(DEFAULTS), specs)]
    for params in grid[::13] + DOMAIN_ENDPOINTS + [DEFAULTS]:
        full = _apply(df, params)
        decisions = np.flatnonzero(full["signal"].to_numpy() != 0)
        ends = sorted({int(i) + 1 for i in decisions[decisions >= 200][:2]} | {300, 520, 700})
        for end in ends:
            for part in (df.iloc[:end], df.iloc[end - 200:end], df.iloc[end - _need(params):end]):
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)


def test_a_200_row_input_with_a_forming_row_decides_on_199_closed_rows_at_the_longest_span():
    params = dict(DEFAULTS, range_period=6, smoothing_period=24, signal_period=24, recursion_length=192)
    df = _frozen("BTC 4h", 400)
    full = _apply(df, params)
    closed = df.iloc[-201:-1]
    assert len(closed.iloc[1:]) == 199
    got = _apply(closed.iloc[1:], params)
    assert got["smi_reason"].iloc[-1] != "insufficient_history"
    pd.testing.assert_series_equal(got[DECISION_COLUMNS].iloc[-1], full[DECISION_COLUMNS].iloc[-2],
                                   check_names=False)
    assert _apply(closed.iloc[2:], params)["smi_reason"].iloc[-1] == "insufficient_history"


def _runtime(df, kind, cutoff=None, boundary=None):
    out = df.copy()
    declare_frame_cadence(out, "4h", kind)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


@pytest.mark.parametrize("kind", [TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE])
def test_check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar(kind):
    base = _walk(900, 11)
    hist = _apply(base, DEFAULTS)
    i = _first(hist, 1)
    df = base.iloc[:i + 1].copy()
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        df.index = df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1)
    last_ts = int(df.index[-1].value // 1_000_000)
    boundary = last_ts + FOUR_H if kind == TIMESTAMP_KIND_OPEN else last_ts + 1
    cases = [
        (boundary, boundary, 1, "entry_long"),
        (boundary + FOUR_H - 1, boundary, 1, "entry_long"),
        (boundary + FOUR_H, boundary, 0, "decision_bar_stale"),
        (boundary - 1, boundary, 0, "decision_bar_future"),
        (boundary + 5, boundary - FOUR_H, 0, "decision_boundary_mismatch"),
        (None, None, 0, "decision_time_missing"),
    ]
    for cutoff, declared, signal, reason in cases:
        out = _apply(_runtime(df, kind, cutoff, declared), DEFAULTS)
        assert int(out["signal"].iloc[-1]) == signal, (cutoff, declared)
        assert out["smi_reason"].iloc[-1] == reason
        assert out["smi_value"].iloc[-1] == hist["smi_value"].iloc[i]
        assert int(out["signal"].iloc[-2]) == int(hist["signal"].iloc[i - 1])


def _composer(df, params, side="", position_ctx=None, closes=("tiered_tp_pct",), open_name=NAME):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, open_name, open_name, list(closes), side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": TIERS} if "tiered_tp_pct" in closes else None)
    return evaluation, finalize_decision(evaluation, side)


def _entry_frame(side):
    base = _walk(900, 11)
    i = _first(_apply(base, DEFAULTS), 1 if side == "long" else -1)
    return base.iloc[:i + 1].copy()


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    _, flat_long = _composer(_entry_frame("long"), DEFAULTS)
    _, flat_short = _composer(_entry_frame("short"), DEFAULTS)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    for frame in (_entry_frame("long"), _entry_frame("short")):
        mark = float(frame["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            _, held = _composer(frame, DEFAULTS, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("mutation", ["geometry", "cadence", "stale", "insufficient", "zero_range"])
def test_invalid_entry_frame_keeps_the_explicit_close_owner_executable(mutation):
    df = _walk(300, 9)
    if mutation == "geometry":
        df = _set_last(df, "open", df["high"].iloc[-1] * 2)
    elif mutation == "cadence":
        df = _shift_index(df, len(df) - 1, pd.Timedelta(hours=2))
    elif mutation == "stale":
        last = int(df.index[-1].value // 1_000_000)
        df = _runtime(df, TIMESTAMP_KIND_OPEN, last + 3 * FOUR_H, last + FOUR_H)
    elif mutation == "insufficient":
        df = df.iloc[-(NEED - 1):]
    else:
        df = df.assign(open=100.0, high=100.0, low=100.0, close=100.0)
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, DEFAULTS, "long", ctx)
    assert evaluation.open_result_df["smi_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, decision = _composer(df, DEFAULTS, "short", dict(ctx, side="short", avg_cost=mark * 1.05))
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    _, decision = _composer(df, DEFAULTS, "long", dict(ctx, avg_cost=mark / 1.01))
    assert decision["close_fraction"] == 0.0 and decision["signal"] == 0
    _, decision = _composer(df, DEFAULTS, "", None)
    assert decision["signal"] == 0 and decision["open_action"] == "none"


def test_close_and_stop_owners_pass_strict_capability_validation():
    report = validate_close_capabilities(close_refs=CLOSE, comparison_mode=None, platform="hyperliquid",
                                         strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    res = _bt(DEFAULTS).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"} and "tiered_tp_pct" in reasons and "sl" in reasons
    by_entry = {}
    for t in trades:
        by_entry.setdefault((t["entry_date"], t["side"]), []).append(t)
    spans = []
    for (entry_date, side), legs in by_entry.items():
        entry = pd.Timestamp(entry_date)
        pos = sig.index.get_loc(entry)
        assert int(sig["signal"].iloc[pos - 1]) == (1 if side == "long" else -1)
        assert legs[0]["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        assert len(legs) == 1
        spans.append((entry, max(pd.Timestamp(leg["exit_date"]) for leg in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit


def test_simulator_long_only_arm_never_opens_shorts():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    res = _bt(DEFAULTS, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h",
                                              save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    df = _walk(1200, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    clean = _bt(DEFAULTS).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    entry = min(pd.Timestamp(t["entry_date"]) for t in clean["trades"]
                if sig.index.get_loc(pd.Timestamp(t["entry_date"])) > 250)
    first = sig.index.get_loc(entry) - 1
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("open")] = corrupt["high"].iloc[first + 1:] * 2
    bad = _apply(corrupt, DEFAULTS)
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    bad = ensure_atr_indicator(bad.assign(open=df["open"]))
    res = _bt(DEFAULTS).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [DEFAULTS])
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 420), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    cfg = _cfg(DEFAULTS, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 900), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    cfg = _cfg(DEFAULTS, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 480), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] != "none").any()
