import contextlib
import importlib.util
import json
import math
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

import eval_windows
from atr import ensure_atr_indicator
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import (
    StrategyReference,
    evaluate_open_close,
    finalize_decision,
    preflight_reference_params,
)

NAME = "mass_index_reversal"
REF = "backtest/candidates/mass_index_reversal_1670/REPORT.md"
STUDY = os.path.join(_REPO, "backtest", "candidates", "mass_index_reversal_1670", "run_study.py")
H = 3_600_000
SEED = {"ema_period": 9, "sum_period": 25, "upper_threshold": 27.0, "lower_threshold": 26.5, "trend_period": 50,
        "slope_span": 5, "confirm_lookback": 5, "max_bulge_age": 50, "recursion_length": 72, "bulge_filter": 1}
NEED = 148
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
EVENT_COLS = ("signal", "mir_valid", "mir_reason", "mir_eval_ts_ms", "mir_range", "mir_ema1", "mir_ema2", "mir_ratio",
              "mir_mass_index", "mir_prev_mass_index", "mir_trend_sma", "mir_trend_slope", "mir_upper_cross",
              "mir_lower_cross", "mir_bulge_armed_ts_ms", "mir_bulge_age", "mir_bulge_slope", "mir_bulge_direction",
              "mir_bulge_completed", "mir_tested_side", "mir_confirmed", "mir_filtered", "mir_close",
              "mir_confirm_high", "mir_confirm_low")
UP_AT, DOWN_AT = 205, 217


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


def _apply(df, **over):
    return _reg().apply_strategy(NAME, df, dict(SEED, **over))


def _frame(rng, mid, start="2026-01-01", kind=TIMESTAMP_KIND_OPEN):
    rng = np.asarray(rng, dtype=float)
    mid = np.asarray(mid, dtype=float)
    df = pd.DataFrame({"open": mid, "high": mid + rng / 2, "low": mid - rng / 2, "close": mid,
                       "volume": 100.0}, index=pd.date_range(start, periods=len(mid), freq="1h"))
    df.index.name = "datetime"
    return declare_frame_cadence(df, "1h", kind)


def _burst(n=420, at=200, width=8, mult=3.0, slope=0.05, extra=()):
    rng = np.ones(n)
    rng[at:at + width] = mult
    for a, w, m in extra:
        rng[a:a + w] = m
    mid = 100.0 + slope * np.arange(n)
    return rng, mid


def _walk(n, seed, bursts=True):
    rs = np.random.default_rng(seed)
    t = np.arange(n)
    drift = np.where(((t + 8) // 80) % 2 == 0, 0.6, -0.6)
    close = 200.0 + np.cumsum(drift + rs.normal(0.0, 0.3, n))
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rs.normal(0.0, 0.3, n)) + 0.4
    if bursts:
        span = np.where(((t % 80) >= 60) & ((t % 80) < 70), span * 4.0, span)
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": rs.uniform(100.0, 1000.0, n)},
                      index=pd.date_range("2026-01-01", periods=n, freq="1h"))
    df.index.name = "datetime"
    return declare_frame_cadence(df, "1h", TIMESTAMP_KIND_OPEN)


def _reference(df, params):
    high = df["high"].to_numpy(dtype=float)
    low = df["low"].to_numpy(dtype=float)
    close = df["close"].to_numpy(dtype=float)
    n = len(df)
    e, p, length = params["ema_period"], params["sum_period"], params["recursion_length"]
    trend, span, look, age_max = (params["trend_period"], params["slope_span"], params["confirm_lookback"],
                                  params["max_bulge_age"])
    upper, lower = params["upper_threshold"], params["lower_threshold"]
    need = max(age_max + length + p + 1, age_max + trend + span, look + 1)
    a = 2.0 / (e + 1.0)
    ema1 = [math.nan] * n
    ema2 = [math.nan] * n
    ratio = [math.nan] * n
    for x in range(length, n):
        s1 = high[x - length] - low[x - length]
        s2 = s1
        for k in range(1, length + 1):
            s1 = a * (high[x - length + k] - low[x - length + k]) + (1.0 - a) * s1
            s2 = a * s1 + (1.0 - a) * s2
        ema1[x], ema2[x], ratio[x] = s1, s2, s1 / s2
    mass = [math.nan] * n
    for x in range(length + p - 1, n):
        total = ratio[x - p + 1]
        for j in range(1, p):
            total = total + ratio[x - p + 1 + j]
        mass[x] = total
    sma = [math.nan] * n
    for x in range(trend - 1, n):
        total = close[x - trend + 1]
        for j in range(1, trend):
            total = total + close[x - trend + 1 + j]
        sma[x] = total / float(trend)
    slope = [math.nan] * n
    for x in range(trend - 1 + span, n):
        slope[x] = sma[x] - sma[x - span]
    rows = []
    state = None
    for t in range(n):
        up = t >= 1 and not math.isnan(mass[t - 1]) and mass[t - 1] <= upper < mass[t]
        down = t >= 1 and not math.isnan(mass[t - 1]) and mass[t - 1] >= lower > mass[t]
        if state is not None and t - state[0] > age_max:
            state = None
        side = 0
        completed = False
        if params["bulge_filter"] == 1:
            if state is not None and down:
                completed = True
                side = state[1]
                state = None
        elif not math.isnan(slope[t]):
            side = 1 if slope[t] < 0 else (-1 if slope[t] > 0 else 0)
        signal = 0
        if t >= look and side:
            if side == 1 and close[t] > max(high[t - look:t]):
                signal = 1
            if side == -1 and close[t] < min(low[t - look:t]):
                signal = -1
        if up:
            state = (t, 1 if slope[t] < 0 else -1) if slope[t] != 0.0 else None
        valid = t >= need - 1
        rows.append({"valid": valid, "signal": signal if valid else 0, "completed": completed and valid,
                     "ema1": ema1[t], "ema2": ema2[t], "ratio": ratio[t], "mass": mass[t], "up": up, "down": down,
                     "slope": slope[t]})
    return rows


def _same(a, b):
    if isinstance(a, str) or isinstance(b, str):
        return a == b
    a, b = float(a), float(b)
    return (math.isnan(a) and math.isnan(b)) or a == b


def test_registration_is_futures_only_bidirectional_hidden_no_edge_with_a_valid_grid():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    assert entry["default_params"] == SEED
    assert entry["edge_status"] == "no_edge" and entry["edge_ref"] == REF
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES and NAME not in fut.list_strategies()
    assert NAME in fut._registry.PLATFORM_ORDER["futures"] and NAME not in fut._registry.PLATFORM_ORDER["spot"]
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    grid = DEFAULT_PARAM_RANGES[NAME]
    assert grid["bulge_filter"] == [1]
    for key, value in SEED.items():
        assert value in grid[key], key
    combos = generate_param_grid(grid)
    assert len(combos) == 324
    for combo in combos:
        fut._registry.validate_params(NAME, combo)
    out = _apply(_frame(*_burst()))
    assert out["mir_required_history"].iloc[0] == NEED


def test_every_bar_matches_an_independent_sequential_reference_in_both_modes():
    df = _walk(960, seed=3)
    for flag in (1, 0):
        params = dict(SEED, bulge_filter=flag)
        out = _apply(df, bulge_filter=flag)
        ref = _reference(df, params)
        for i, want in enumerate(ref):
            rec = out.iloc[i]
            assert (rec["mir_valid"] == 1.0) == want["valid"], (flag, i, rec["mir_reason"])
            assert int(rec["signal"]) == want["signal"], (flag, i, rec["mir_reason"])
            if want["valid"]:
                for col, key in (("mir_ema1", "ema1"), ("mir_ema2", "ema2"), ("mir_ratio", "ratio"),
                                 ("mir_mass_index", "mass"), ("mir_trend_slope", "slope")):
                    assert rec[col] == pytest.approx(want[key], rel=1e-12, abs=1e-12), (flag, i, col)
                assert rec["mir_upper_cross"] == float(want["up"]) and rec["mir_lower_cross"] == float(want["down"])
                if flag == 1:
                    assert rec["mir_bulge_completed"] == float(want["completed"]), i
        assert (out["signal"] == 1).any() and (out["signal"] == -1).any(), flag
    on = _apply(df)
    assert (on["mir_bulge_completed"] == 1.0).sum() > (on["signal"] != 0).sum() > 0


def test_both_smoothing_passes_ratio_orientation_and_rolling_sum():
    out = _apply(_frame(*_burst()))
    i = UP_AT
    rec = out.iloc[i]
    rng = (out["high"] - out["low"]).to_numpy()
    a = 2.0 / 10.0
    e1 = rng[i - 72]
    e2 = e1
    for k in range(1, 73):
        e1 = a * rng[i - 72 + k] + (1 - a) * e1
        e2 = a * e1 + (1 - a) * e2
    assert rec["mir_ema1"] == e1 and rec["mir_ema2"] == e2 and rec["mir_ratio"] == e1 / e2
    assert rec["mir_ratio"] > 1.0
    assert rec["mir_mass_index"] == pytest.approx(out["mir_ratio"].iloc[i - 24:i + 1].sum(), rel=1e-12)
    assert out["mir_mass_index"].iloc[190] == pytest.approx(25.0, rel=1e-12)
    assert out["mir_ratio"].iloc[DOWN_AT] < 1.0


def test_upper_crossing_arms_and_the_first_lower_crossing_completes_once():
    out = _apply(_frame(*_burst()))
    assert list(np.flatnonzero(out["mir_upper_cross"] == 1.0)) == [UP_AT]
    assert list(np.flatnonzero(out["mir_lower_cross"] == 1.0)) == [DOWN_AT]
    assert (out["mir_reason"].iloc[UP_AT + 1:DOWN_AT] == "armed").all()
    assert out["mir_reason"].iloc[UP_AT] == "no_bulge"
    assert out["mir_bulge_age"].iloc[DOWN_AT] == DOWN_AT - UP_AT
    assert out["mir_bulge_armed_ts_ms"].iloc[DOWN_AT] == float(out.index[UP_AT].value // 1_000_000)
    assert out["mir_reason"].iloc[DOWN_AT] == "confirmation_failed" and out["mir_filtered"].iloc[DOWN_AT] == 1.0
    assert (out["mir_reason"].iloc[DOWN_AT + 1:] == "no_bulge").all()
    assert int(out["signal"].abs().sum()) == 0


def _short_entry_mid(slope=0.05):
    rng, mid = _burst(slope=slope)
    mid = mid.copy()
    mid[DOWN_AT:] = mid[DOWN_AT:] - 5.0 * np.sign(slope)
    return rng, mid


def test_rising_and_falling_slopes_give_mirrored_entries_on_the_completion_bar():
    short = _apply(_frame(*_short_entry_mid(0.05)))
    assert int(short["signal"].iloc[DOWN_AT]) == -1 and short["mir_reason"].iloc[DOWN_AT] == "entry_short"
    assert short["mir_bulge_direction"].iloc[DOWN_AT] == -1.0 and short["mir_bulge_slope"].iloc[DOWN_AT] > 0
    assert int(short["signal"].abs().sum()) == 1
    long_ = _apply(_frame(*_short_entry_mid(-0.05)))
    assert int(long_["signal"].iloc[DOWN_AT]) == 1 and long_["mir_reason"].iloc[DOWN_AT] == "entry_long"
    assert long_["mir_bulge_direction"].iloc[DOWN_AT] == 1.0 and long_["mir_bulge_slope"].iloc[DOWN_AT] < 0
    assert int(long_["signal"].abs().sum()) == 1


def test_direction_is_frozen_at_the_upper_crossing():
    rng, mid = _burst(slope=0.05)
    mid = mid.copy()
    mid[UP_AT + 1:] = mid[UP_AT] - 2.0 * np.arange(1, len(mid) - UP_AT)
    out = _apply(_frame(rng, mid))
    assert out["mir_trend_slope"].iloc[DOWN_AT] < 0 and out["mir_bulge_slope"].iloc[DOWN_AT] > 0
    assert int(out["signal"].iloc[DOWN_AT]) == -1


def test_price_confirmation_is_strict_against_the_previous_highs_and_lows():
    rng, mid = _burst(slope=0.05)
    df = _frame(rng, mid)
    lows = df["low"].iloc[DOWN_AT - 5:DOWN_AT].min()
    at = df.copy()
    at.attrs = dict(df.attrs)
    at.iloc[DOWN_AT, at.columns.get_loc("close")] = lows
    at.iloc[DOWN_AT, at.columns.get_loc("open")] = lows
    at.iloc[DOWN_AT, at.columns.get_loc("low")] = lows - 0.5
    at.iloc[DOWN_AT, at.columns.get_loc("high")] = lows + 0.5
    out = _apply(at)
    assert out["mir_reason"].iloc[DOWN_AT] == "confirmation_failed"
    below = at.copy()
    below.attrs = dict(df.attrs)
    for col, off in (("close", 0.0), ("open", 0.0), ("low", -0.5), ("high", 0.5)):
        below.iloc[DOWN_AT, below.columns.get_loc(col)] = lows - 1e-6 + off
    assert int(_apply(below)["signal"].iloc[DOWN_AT]) == -1


def test_threshold_equality_boundaries():
    df = _frame(*_short_entry_mid())
    base = _apply(df)
    prev_up, cur_up = base["mir_prev_mass_index"].iloc[UP_AT], base["mir_mass_index"].iloc[UP_AT]
    prev_dn, cur_dn = base["mir_prev_mass_index"].iloc[DOWN_AT], base["mir_mass_index"].iloc[DOWN_AT]
    assert int(_apply(df, upper_threshold=float(prev_up))["signal"].iloc[DOWN_AT]) == -1
    equal_cur = _apply(df, upper_threshold=float(cur_up))
    assert equal_cur["mir_upper_cross"].iloc[UP_AT] == 0.0
    assert int(_apply(df, lower_threshold=float(prev_dn))["signal"].iloc[DOWN_AT]) == -1
    equal_dn = _apply(df, lower_threshold=float(cur_dn))
    assert equal_dn["mir_lower_cross"].iloc[DOWN_AT] == 0.0 and int(equal_dn["signal"].iloc[DOWN_AT]) == 0


def test_age_limit_is_eligible_at_the_limit_and_expires_one_bar_later():
    df = _frame(*_short_entry_mid())
    age = DOWN_AT - UP_AT
    at_limit = _apply(df, max_bulge_age=age)
    assert int(at_limit["signal"].iloc[DOWN_AT]) == -1 and at_limit["mir_bulge_age"].iloc[DOWN_AT] == age
    expired = _apply(df, max_bulge_age=age - 1)
    assert int(expired["signal"].abs().sum()) == 0
    assert expired["mir_reason"].iloc[DOWN_AT] == "no_bulge"
    assert expired["mir_reason"].iloc[DOWN_AT - 1] == "armed"


def test_a_newer_crossing_replaces_the_older_bulge_and_a_zero_slope_replacement_leaves_none():
    rng, mid = _burst(extra=((262, 8, 3.0),))
    over = {"lower_threshold": 20.0, "max_bulge_age": 100}
    out = _apply(_frame(rng, mid), **over)
    ups = list(np.flatnonzero(out["mir_upper_cross"] == 1.0))
    assert len(ups) == 2 and ups[0] == UP_AT
    second = ups[1]
    assert out["mir_bulge_armed_ts_ms"].iloc[second - 1] == float(out.index[UP_AT].value // 1_000_000)
    assert out["mir_bulge_armed_ts_ms"].iloc[second + 1] == float(out.index[second].value // 1_000_000)
    assert out["mir_bulge_age"].iloc[second + 1] == 1.0
    flat = mid.copy()
    flat[UP_AT + 1:] = flat[UP_AT]
    zero = _apply(_frame(rng, flat), **over)
    assert zero["mir_trend_slope"].iloc[UP_AT] > 0 and zero["mir_reason"].iloc[second - 1] == "armed"
    assert zero["mir_trend_slope"].iloc[second] == 0.0 and zero["mir_upper_cross"].iloc[second] == 1.0
    tail = zero.iloc[second + 1:second + 20]
    assert (tail["mir_bulge_direction"] == 0.0).all() and set(tail["mir_reason"]) == {"zero_slope"}
    assert int(zero["signal"].abs().sum()) == 0


def test_range_expansion_alone_never_opens_a_position():
    rng = np.ones(320)
    rng[200:] = 3.0
    df = _frame(rng, 100.0 - 0.05 * np.arange(320))
    out = _apply(df)
    assert (out["mir_upper_cross"] == 1.0).sum() == 1
    done = out["mir_bulge_completed"] == 1.0
    assert done.sum() == 1 and set(out.loc[done, "mir_reason"]) == {"confirmation_failed"}
    assert int(out["signal"].abs().sum()) == 0
    up = int(np.flatnonzero(out["mir_upper_cross"] == 1.0)[0])
    cut = df.iloc[:up + 6].copy()
    cut.attrs = dict(df.attrs)
    early = _apply(cut)
    assert early["mir_reason"].iloc[-1] == "armed" and int(early["signal"].abs().sum()) == 0


def test_filter_off_tests_every_bar_with_the_current_slope():
    df = _walk(480, seed=9)
    off = _apply(df, bulge_filter=0)
    on = _apply(df)
    valid = off["mir_valid"] == 1.0
    slope = off["mir_trend_slope"][valid]
    side = off["mir_tested_side"][valid]
    assert ((slope < 0) == (side == 1)).all() and ((slope > 0) == (side == -1)).all()
    assert (off["signal"] != 0).sum() > (on["signal"] != 0).sum()
    assert set(off["mir_reason"][valid]) <= {"entry_long", "entry_short", "confirmation_failed", "zero_slope"}


INVALID_PARAMS = [
    ({"ema_period": 1}, "ema_period"),
    ({"ema_period": 9.5}, "ema_period"),
    ({"ema_period": True}, "ema_period"),
    ({"sum_period": 0}, "sum_period"),
    ({"trend_period": 1}, "trend_period"),
    ({"slope_span": 0}, "slope_span"),
    ({"confirm_lookback": 0}, "confirm_lookback"),
    ({"max_bulge_age": 0}, "max_bulge_age"),
    ({"recursion_length": 71}, "recursion_length"),
    ({"upper_threshold": 26.5}, "lower_threshold"),
    ({"lower_threshold": 0.0}, "lower_threshold"),
    ({"upper_threshold": float("nan")}, "upper_threshold"),
    ({"upper_threshold": float("inf")}, "upper_threshold"),
    ({"upper_threshold": "27"}, "upper_threshold"),
    ({"lower_threshold": False}, "lower_threshold"),
    ({"bulge_filter": 2}, "bulge_filter"),
    ({"bulge_filter": True}, "bulge_filter"),
    ({"max_bulge_age": 102}, "required history"),
    ({"trend_period": 140, "max_bulge_age": 55}, "required history"),
    ({"unknown_knob": 1}, "unknown parameters"),
]


def test_invalid_parameters_are_refused_by_the_registry_and_the_evaluator():
    for override, needle in INVALID_PARAMS:
        with _case(override):
            with pytest.raises(ValueError, match=needle):
                _reg()._registry.validate_params(NAME, override)
            if "unknown_knob" not in override:
                with pytest.raises(ValueError):
                    _apply(_frame(*_burst()), **override)


def test_the_largest_allowed_history_is_199_rows():
    _reg()._registry.validate_params(NAME, {"max_bulge_age": 101})
    out = _apply(_frame(*_burst()), max_bulge_age=101)
    assert out["mir_required_history"].iloc[0] == 199.0


def _with_index(df, idx):
    out = df.copy()
    out.index = pd.DatetimeIndex(idx, name="datetime")
    out.attrs = dict(df.attrs)
    return out


def _set(df, pos, col, value):
    out = df.copy()
    out.attrs = dict(df.attrs)
    out.iloc[pos, out.columns.get_loc(col)] = value
    return out


DEFECT_AT = DOWN_AT - 20


DEFECT_REASONS = {"nonfinite_input", "nonpositive_price", "invalid_candle_geometry", "negative_volume",
                  "timestamp_order", "cadence_gap", "missing_timestamps"}


IN_WINDOW_DEFECTS = [
    (lambda df: _set(df, DEFECT_AT, "close", np.nan), "nonfinite_input", 0),
    (lambda df: _set(df, DEFECT_AT, "high", np.inf), "nonfinite_input", 0),
    (lambda df: _set(df, DEFECT_AT, "low", 0.0), "nonpositive_price", 0),
    (lambda df: _set(df, DEFECT_AT, "open", 500.0), "invalid_candle_geometry", 0),
    (lambda df: _set(df, DEFECT_AT, "high", 1.0), "invalid_candle_geometry", 0),
    (lambda df: _set(df, DEFECT_AT, "volume", -1.0), "negative_volume", 0),
    (lambda df: _with_index(df, list(df.index[:DEFECT_AT]) + [df.index[DEFECT_AT - 1]]
                            + list(df.index[DEFECT_AT + 1:])), "timestamp_order", 0),
    (lambda df: _with_index(df, list(df.index[:DEFECT_AT]) + [df.index[DEFECT_AT + 1], df.index[DEFECT_AT]]
                            + list(df.index[DEFECT_AT + 2:])), "timestamp_order", 1),
    (lambda df: _with_index(df, list(df.index[:DEFECT_AT]) + list(df.index[DEFECT_AT:] + pd.Timedelta(hours=1))),
     "cadence_gap", 0),
    (lambda df: _with_index(df, list(df.index[:DEFECT_AT]) + [pd.NaT] + list(df.index[DEFECT_AT + 1:])),
     "missing_timestamps", 0),
]


def test_defects_inside_the_required_window_hold_and_recover_after_a_clean_window():
    for i, (mutate, reason, at) in enumerate(IN_WINDOW_DEFECTS):
        with _case(f"{i}-{reason}-{at}"):
            _defects_inside_the_required_window_hold_and_recover_after_a_clean_window(mutate, reason, at)


def _defects_inside_the_required_window_hold_and_recover_after_a_clean_window(mutate, reason, at):
    clean = _frame(*_short_entry_mid())
    assert int(_apply(clean)["signal"].iloc[DOWN_AT]) == -1
    out = _apply(mutate(clean))
    assert out["mir_reason"].iloc[DEFECT_AT + at] == reason
    assert out["mir_reason"].iloc[DOWN_AT] in DEFECT_REASONS and int(out["signal"].iloc[DOWN_AT]) == 0
    assert out["mir_valid"].iloc[DOWN_AT] == 0.0 and np.isnan(out["mir_mass_index"].iloc[DOWN_AT])
    held = out.iloc[DEFECT_AT:DEFECT_AT + NEED - 1]
    assert held["mir_valid"].eq(0.0).all()
    assert out["mir_valid"].iloc[DEFECT_AT + NEED + 2] == 1.0


def test_received_order_defects_hold_like_a_sort_repair():
    clean = _frame(*_short_entry_mid())
    flagged = clean.copy()
    flagged.attrs = dict(clean.attrs)
    flagged.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(clean.index[DEFECT_AT].value // 1_000_000)]
    out = _apply(flagged)
    assert out["mir_reason"].iloc[DOWN_AT] == "timestamp_order" and int(out["signal"].abs().sum()) == 0


def test_defects_outside_the_required_window_never_change_the_decision():
    clean = _frame(*_short_entry_mid())
    base = _apply(clean)
    first = DOWN_AT - NEED + 1
    for pos in (0, 10, first - 1):
        out = _apply(_set(clean, pos, "close", np.nan))
        for col in EVENT_COLS:
            assert _same(out[col].iloc[DOWN_AT], base[col].iloc[DOWN_AT]), (pos, col)
    gap = _with_index(clean, list(clean.index[:first]) + list(clean.index[first:] + pd.Timedelta(hours=3)))
    out = _apply(gap)
    assert out["mir_valid"].iloc[DOWN_AT] == 1.0 and int(out["signal"].iloc[DOWN_AT]) == -1
    edge = _with_index(clean, list(clean.index[:first + 1]) + list(clean.index[first + 1:] + pd.Timedelta(hours=3)))
    assert _apply(edge)["mir_reason"].iloc[DOWN_AT] == "cadence_gap"


def test_insufficient_history_and_frame_level_holds():
    df = _frame(*_short_entry_mid())
    tail = df.iloc[DOWN_AT - NEED + 1:DOWN_AT + 1].copy()
    tail.attrs = dict(df.attrs)
    assert int(_apply(tail)["signal"].iloc[-1]) == -1
    short = tail.iloc[1:].copy()
    short.attrs = dict(df.attrs)
    got = _apply(short)
    assert set(got["mir_reason"]) == {"insufficient_history"} and int(got["signal"].abs().sum()) == 0
    undeclared = df.copy()
    undeclared.attrs = {}
    assert set(_apply(undeclared)["mir_reason"]) == {"missing_cadence"}
    bad = df.copy()
    bad.attrs = {FRAME_INTERVAL_ATTR: True}
    assert set(_apply(bad)["mir_reason"]) == {"missing_cadence"}
    assert set(_apply(df.drop(columns=["volume"]))["mir_reason"]) == {"missing_columns"}
    assert set(_apply(df.reset_index(drop=True))["mir_reason"]) == {"missing_timestamps"}


def test_a_flat_range_window_holds_on_a_nonpositive_second_average():
    rng, mid = _burst()
    rng = rng.copy()
    rng[100:260] = 0.0
    out = _apply(_frame(rng, mid))
    assert out["mir_reason"].iloc[250] == "nonpositive_average" and int(out["signal"].abs().sum()) == 0


def test_native_close_and_timezone_inputs_decide_the_same():
    df = _frame(*_short_entry_mid())
    base = _apply(df)
    native = df.copy()
    native.index = native.index + pd.Timedelta(milliseconds=H - 1)
    native.attrs = dict(df.attrs)
    native.attrs[FRAME_TIMESTAMP_KIND_ATTR] = TIMESTAMP_KIND_NATIVE_CLOSE
    ny = df.copy()
    ny.index = ny.index.tz_localize("UTC").tz_convert("America/New_York")
    ny.attrs = dict(df.attrs)
    for variant in (native, ny):
        got = _apply(variant)
        for col in EVENT_COLS:
            assert all(_same(a, b) for a, b in zip(got[col], base[col])), col


def test_full_series_every_prefix_and_bounded_tails_agree():
    df = _walk(560, seed=11)
    for flag in (1, 0):
        full = _apply(df, bulge_filter=flag)
        valid = full["mir_valid"].to_numpy() == 1.0
        assert (full["signal"] != 0).any()
        for i in range(len(df)):
            frames = [df.iloc[:i + 1]]
            if valid[i]:
                frames += [df.iloc[i + 1 - size:i + 1] for size in (NEED, 199, 200) if i + 1 >= size]
            for frame in frames:
                frame = frame.copy()
                frame.attrs = dict(df.attrs)
                got = _apply(frame, bulge_filter=flag).iloc[-1]
                for col in EVENT_COLS:
                    assert _same(got[col], full[col].iloc[i]), (flag, i, len(frame), col)


def test_check_preflight_refuses_invalid_open_and_close_fallback_parameters():
    reg = _reg()
    refs = [StrategyReference("open", "breakout"), StrategyReference("close", NAME)]
    preflight_reference_params(refs, {}, {NAME: {}}, reg.get_strategy, reg.validate_params, timeframe="1h")
    with pytest.raises(ValueError) as exc:
        preflight_reference_params(refs, {}, {NAME: {"recursion_length": 40}}, reg.get_strategy,
                                   reg.validate_params, timeframe="1h")
    assert str(exc.value).startswith(f"close strategy '{NAME}' has invalid parameters")
    open_refs = [StrategyReference("open", NAME)]
    with pytest.raises(ValueError, match=f"open strategy '{NAME}' has invalid parameters"):
        preflight_reference_params(open_refs, {"bulge_filter": True}, {}, reg.get_strategy, reg.validate_params)


def test_research_legs_refuse_invalid_parameters_before_loading_candles(monkeypatch):
    import data_fetcher

    def refuse(*_a, **_k):
        raise AssertionError("candles were loaded before the parameters were validated")

    monkeypatch.setattr(data_fetcher, "load_cached_data", refuse)
    with pytest.raises(ValueError, match="recursion_length"):
        eval_windows.run_leg(_reg(), NAME, {"recursion_length": 40}, "BTC/USDT", "1h", ("2026-01-01", "2026-02-01"))


def _composer(df, side="", position_ctx=None, protection_df=None, params=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side,
        params=dict(params or SEED), position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": CLOSE[0]["params"]}, protection_df=protection_df)
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    short_df = _frame(*_short_entry_mid(0.05)).iloc[:DOWN_AT + 1].copy()
    long_df = _frame(*_short_entry_mid(-0.05)).iloc[:DOWN_AT + 1].copy()
    _, s = _composer(short_df)
    _, lg = _composer(long_df)
    assert (s["open_action"], s["signal"]) == ("short", -1)
    assert (lg["open_action"], lg["signal"]) == ("long", 1)
    for frame in (short_df, long_df):
        mark = float(frame["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 1.0, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            _, held = _composer(frame, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


def test_entry_holds_keep_the_close_owner_executable():
    for mutation in ("gap", "nan_close", "undeclared", "short_history", "unavailable"):
        with _case(mutation):
            _entry_holds_keep_the_close_owner_executable(mutation)


def _entry_holds_keep_the_close_owner_executable(mutation):
    df = _frame(*_short_entry_mid(0.05)).iloc[:DOWN_AT + 1].copy()
    protection = None
    if mutation == "gap":
        df = _with_index(df, list(df.index[:-3]) + list(df.index[-3:] + pd.Timedelta(hours=1)))
    elif mutation == "nan_close":
        df = _set(df, len(df) - 4, "close", np.nan)
    elif mutation == "undeclared":
        df.attrs = {}
    elif mutation == "short_history":
        tail = df.iloc[-NEED + 1:].copy()
        tail.attrs = dict(df.attrs)
        df = tail
    else:
        protection, df = df, None
    frame = df if df is not None else protection
    mark = float(frame["close"].dropna().iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, "long", ctx, protection_df=protection)
    if df is not None:
        assert evaluation.open_result_df["mir_valid"].iloc[-1] == 0.0
        assert int(evaluation.open_result_df["signal"].iloc[-1]) == 0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, flat = _composer(df, "", None, protection_df=protection)
    assert flat["signal"] == 0 and flat["open_action"] == "none"


def test_close_and_stop_owners_pass_strict_capability_validation():
    report = validate_close_capabilities(close_refs=CLOSE, comparison_mode=None, platform="hyperliquid",
                                         strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]
    assert report["incomplete_parity"] is False


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": dict(SEED)},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_one_close_and_one_stop_and_no_reversal_or_scale_in():
    df = _walk(2400, seed=31)
    sig = ensure_atr_indicator(_apply(df))
    assert (sig["signal"] == 1).sum() >= 3 and (sig["signal"] == -1).sum() >= 3
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"}
    spans = []
    for t in trades:
        entry = pd.Timestamp(t["entry_date"])
        pos = sig.index.get_loc(entry)
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        spans.append((entry, pd.Timestamp(t["exit_date"])))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit


def _cfg(**kw):
    base = dict(strategy_name=NAME, params=dict(SEED), registry="futures", platform="hyperliquid", symbol="BTC",
                timeframe="1h", close_refs=json.loads(json.dumps(CLOSE)), direction="both",
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [200, 0])
@pytest.mark.parametrize("batched", [False, True])
def test_decision_parity_with_the_close_and_stop_owner(window, batched):
    df = _walk(720, seed=41)
    frame = compute_parity_frame(df, cfg=_cfg(batched=batched), window=window or None)
    result = summarize(frame)
    assert result["bars_compared"] > 0 and result["mismatches"] == 0, frame[~frame["match"]].head()
    assert result["clean"] and result["close_validation"]["close_eligibility"] == "eligible"
    if batched:
        assert result["batch_mismatches"] == 0
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()


def _study():
    spec = importlib.util.spec_from_file_location("_mir_1670_study", STUDY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_study_reference_audit_and_verdict_rules():
    study = _study()
    df = _frame(*_short_entry_mid())
    ref = study.reference_decision(df, DOWN_AT, SEED)
    out = _apply(df)
    assert ref["signal"] == -1 and ref["arm"] == UP_AT and ref["completed"]
    assert ref["mass_index"] == pytest.approx(out["mir_mass_index"].iloc[DOWN_AT], rel=1e-12)
    proto = json.load(open(os.path.join(os.path.dirname(STUDY), "protocol.json")))
    assert proto["seed_params"] == SEED and proto["required_history_bars"] == NEED
    assert study.check_grid(proto)["all_valid"]
    assert len(study.neighbors(SEED)) == 10
    assert study.stability_gate([{"unscorable": [], "net_pnl_usd": 1.0}] * 7
                                + [{"unscorable": [], "net_pnl_usd": -1.0}] * 3)["ok"] is True
    assert study.stability_gate([{"unscorable": [], "net_pnl_usd": 1.0}] * 6
                                + [{"unscorable": [], "net_pnl_usd": -1.0}] * 4)["ok"] is False
    assert study.stability_gate([{"unscorable": [], "net_pnl_usd": 1.0}] * 6
                                + [{"unscorable": ["x"], "net_pnl_usd": 0.0}] * 4)["ok"] is None
    assert study.concentration([10.0, 10.0, -3.0])["ok"] is True
    assert study.concentration([30.0, 10.0, 5.0])["ok"] is False
    assert study.concentration([-1.0])["ok"] is False
