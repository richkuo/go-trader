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
import optimizer
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid, strategy_warmup_bars, warmup_exit_long_entry
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import (
    StrategyReference,
    evaluate_open_close,
    finalize_decision,
    preflight_reference_params,
)

NAME = "elder_force_pullback"
REF = "backtest/candidates/elder_force_pullback_1668/REPORT.md"
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "elder_force_pullback_1668")
SEED = {"force_period": 13, "trend_period": 50, "slope_span": 5, "recursion_length": 150, "volume_weighting": 1}
NEED = 152
TIERS = [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
         {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": TIERS}}]
CROSS_AT = 211
FIRST_PULLBACK = 200
EVENT_COLS = ("signal", "efp_valid", "efp_reason", "efp_eval_ts_ms", "efp_close", "efp_prev_close", "efp_volume",
              "efp_prev_volume", "efp_weight", "efp_raw_force", "efp_prev_raw_force", "efp_seed_force",
              "efp_prev_seed_force", "efp_force", "efp_prev_force", "efp_trend_sma", "efp_trend_sma_lag",
              "efp_prev_trend_sma", "efp_prev_trend_sma_lag", "efp_uptrend", "efp_prev_uptrend", "efp_downtrend",
              "efp_prev_downtrend", "efp_cross", "efp_volume_eligible")


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


def _frame(close, volume=None, start="2026-01-01", kind=TIMESTAMP_KIND_OPEN):
    close = np.asarray(close, dtype=float)
    open_ = np.concatenate(([close[0]], close[:-1]))
    vol = np.full(len(close), 100.0) if volume is None else np.asarray(volume, dtype=float)
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + 0.5,
                       "low": np.minimum(open_, close) - 0.5, "close": close, "volume": vol},
                      index=pd.date_range(start, periods=len(close), freq="1h"))
    df.index.name = "datetime"
    return declare_frame_cadence(df, "1h", kind)


def _pullback(sign=1, n=260):
    step = np.full(n, 0.5)
    step[FIRST_PULLBACK:FIRST_PULLBACK + 5] = -2.0
    step[0] = 0.0
    return 100.0 + np.cumsum(step) if sign > 0 else 400.0 - np.cumsum(step)


def _walk(n, seed):
    rs = np.random.default_rng(seed)
    t = np.arange(n)
    drift = np.where((t // 150) % 2 == 0, 0.3, -0.3)
    close = 300.0 + np.cumsum(drift + rs.normal(0.0, 1.0, n))
    return _frame(close, rs.uniform(100.0, 1000.0, n))


def _reference(df, params):
    close = [float(x) for x in df["close"]]
    volume = [float(x) for x in df["volume"]]
    n = len(close)
    p, trend, span, length = (params["force_period"], params["trend_period"], params["slope_span"],
                              params["recursion_length"])
    weighted = params["volume_weighting"] == 1
    a = 2.0 / (p + 1.0)
    raw = [math.nan] + [(close[i] - close[i - 1]) * (volume[i] if weighted else 1.0) for i in range(1, n)]
    force = [math.nan] * n
    for t in range(length, n):
        e = raw[t - length + 1]
        for k in range(t - length + 2, t + 1):
            e = a * raw[k] + (1.0 - a) * e
        force[t] = e
    sma = [math.nan] * n
    for t in range(trend - 1, n):
        total = close[t - trend + 1]
        for j in range(t - trend + 2, t + 1):
            total = total + close[j]
        sma[t] = total / float(trend)
    need = max(length + 2, trend + span + 1)
    rows = []
    for t in range(n):
        valid = t >= need - 1
        sig = 0
        if valid:
            up = all(close[x] > sma[x] > sma[x - span] for x in (t - 1, t))
            down = all(close[x] < sma[x] < sma[x - span] for x in (t - 1, t))
            vol_ok = volume[t] > 0 and volume[t - 1] > 0
            if force[t - 1] < 0 < force[t] and up and vol_ok:
                sig = 1
            elif force[t - 1] > 0 > force[t] and down and vol_ok:
                sig = -1
        rows.append({"valid": valid, "signal": sig, "force": force[t], "sma": sma[t], "raw": raw[t]})
    return rows


def _same(a, b):
    if isinstance(a, str) or isinstance(b, str):
        return a == b
    a, b = float(a), float(b)
    return (math.isnan(a) and math.isnan(b)) or a == b


def _copy(df):
    out = df.copy()
    out.attrs = dict(df.attrs)
    return out


def _set(df, pos, col, value):
    out = _copy(df)
    out.iloc[pos, out.columns.get_loc(col)] = value
    return out


def _with_index(df, idx):
    out = _copy(df)
    out.index = pd.DatetimeIndex(idx, name="datetime")
    return out


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
    assert grid["volume_weighting"] == [1]
    for key, value in SEED.items():
        assert value in grid[key], key
    combos = generate_param_grid(grid)
    assert len(combos) == 54
    for combo in combos:
        fut._registry.validate_params(NAME, combo)
        fut._registry.validate_params(NAME, dict(combo, volume_weighting=0))
    out = _apply(_frame(_pullback()))
    assert out["efp_required_history"].iloc[0] == NEED


def test_fixed_length_helper_reads_exactly_its_window_and_restarts_at_its_own_seed():
    from indicators_core import fixed_length_ema
    values = np.random.default_rng(5).normal(0.0, 3.0, 60)
    out = fixed_length_ema(values, 4, 12)
    assert np.isnan(out[:11]).all()
    a = 2.0 / 5.0
    for t in range(11, 60):
        e = values[t - 11]
        for k in range(t - 10, t + 1):
            e = a * values[k] + (1.0 - a) * e
        assert out[t] == e
    changed = values.copy()
    changed[:20] = 1e6
    again = fixed_length_ema(changed, 4, 12)
    assert np.array_equal(again[31:], out[31:])
    assert not np.array_equal(again[30:31], out[30:31])
    poisoned = values.copy()
    poisoned[25] = np.nan
    held = fixed_length_ema(poisoned, 4, 12)
    assert np.isnan(held[25:37]).all() and np.array_equal(held[37:], out[37:])
    with pytest.raises(ValueError):
        fixed_length_ema(values, True, 12)


def test_every_bar_matches_an_independent_sequential_reference_in_both_weightings():
    df = _walk(900, seed=3)
    for flag in (1, 0):
        params = dict(SEED, volume_weighting=flag)
        out = _apply(df, volume_weighting=flag)
        for i, want in enumerate(_reference(df, params)):
            rec = out.iloc[i]
            assert (rec["efp_valid"] == 1.0) == want["valid"], (flag, i, rec["efp_reason"])
            assert int(rec["signal"]) == want["signal"], (flag, i, rec["efp_reason"])
            if want["valid"]:
                assert rec["efp_force"] == pytest.approx(want["force"], rel=1e-12, abs=1e-12), (flag, i)
                assert rec["efp_trend_sma"] == pytest.approx(want["sma"], rel=1e-12, abs=1e-12), (flag, i)
                assert rec["efp_raw_force"] == pytest.approx(want["raw"], rel=1e-12, abs=1e-12), (flag, i)
        assert (out["signal"] == 1).any() and (out["signal"] == -1).any(), flag
        reasons = set(out["efp_reason"][out["efp_valid"] == 1.0])
        assert {"entry_long", "entry_short", "no_cross", "trend_unconfirmed"} <= reasons, reasons


def test_pullback_recovery_gives_mirrored_entries_on_the_strict_crossing_bar():
    for sign, signal, reason in ((1, 1, "entry_long"), (-1, -1, "entry_short")):
        for flag in (1, 0):
            out = _apply(_frame(_pullback(sign)), volume_weighting=flag)
            assert list(np.flatnonzero(out["signal"] != 0)) == [CROSS_AT], (sign, flag)
            rec = out.iloc[CROSS_AT]
            assert int(rec["signal"]) == signal and rec["efp_reason"] == reason
            assert rec["efp_cross"] == float(signal)
            assert np.sign(rec["efp_prev_force"]) == -signal and np.sign(rec["efp_force"]) == signal
            trend_cols = ("efp_uptrend", "efp_prev_uptrend") if sign > 0 else ("efp_downtrend", "efp_prev_downtrend")
            assert all(rec[c] == 1.0 for c in trend_cols)
            assert rec["efp_volume_eligible"] == 1.0
            assert out["efp_reason"].iloc[FIRST_PULLBACK + 1] == "trend_unconfirmed"


def test_seed_force_and_weight_diagnostics_follow_the_contract():
    df = _frame(_pullback())
    out = _apply(df)
    rec = out.iloc[CROSS_AT]
    close = df["close"].to_numpy()
    seed_bar = CROSS_AT - SEED["recursion_length"] + 1
    assert rec["efp_seed_force"] == (close[seed_bar] - close[seed_bar - 1]) * 100.0
    assert rec["efp_prev_seed_force"] == (close[seed_bar - 1] - close[seed_bar - 2]) * 100.0
    assert rec["efp_weight"] == 100.0 and _apply(df, volume_weighting=0)["efp_weight"].iloc[CROSS_AT] == 1.0
    assert rec["efp_eval_ts_ms"] == float(df.index[CROSS_AT].value // 1_000_000)
    assert rec["efp_interval_ms"] == 3_600_000.0


def test_exact_zero_and_constant_closes_never_trigger():
    flat = _apply(_frame(np.full(260, 150.0)))
    valid = flat["efp_valid"] == 1.0
    assert valid.any() and set(flat.loc[valid, "efp_reason"]) == {"zero_force"}
    assert (flat.loc[valid, "efp_force"] == 0.0).all() and int(flat["signal"].abs().sum()) == 0
    steps = np.zeros(400)
    steps[300:] = 0.5
    close = 100.0 + np.cumsum(steps)
    close[:300] = np.linspace(80.0, 100.0, 300)
    close[100:300] = 100.0
    out = _apply(_frame(close))
    assert out["efp_force"].iloc[299] == 0.0 and out["efp_force"].iloc[300] > 0.0
    assert out["efp_reason"].iloc[300] == "zero_force" and int(out["signal"].iloc[300]) == 0


def test_zero_volume_eligibility_is_identical_in_both_weightings():
    df = _frame(_pullback())
    for pos in (CROSS_AT, CROSS_AT - 1):
        zeroed = _set(df, pos, "volume", 0.0)
        ablation = _apply(zeroed, volume_weighting=0)
        assert ablation["efp_cross"].iloc[CROSS_AT] == 1.0
        assert ablation["efp_reason"].iloc[CROSS_AT] == "volume_ineligible"
        assert int(ablation["signal"].abs().sum()) == 0
        assert ablation["efp_volume_eligible"].iloc[CROSS_AT] == 0.0
        weighted = _apply(zeroed)
        assert int(weighted["signal"].iloc[CROSS_AT]) == 0
    older = _set(df, 190, "volume", 0.0)
    weighted = _apply(older)
    assert weighted["efp_raw_force"].iloc[190] == 0.0
    ablation = _apply(older, volume_weighting=0)
    assert ablation["efp_raw_force"].iloc[190] == df["close"].iloc[190] - df["close"].iloc[189]
    assert int(ablation["signal"].iloc[CROSS_AT]) == 1


def test_trend_needs_both_candles():
    df = _walk(900, seed=3)
    out = _apply(df)
    one_bar = ((out["efp_cross"] == 1.0) & (out["efp_uptrend"] == 1.0) & (out["efp_prev_uptrend"] == 0.0)) | (
        (out["efp_cross"] == -1.0) & (out["efp_downtrend"] == 1.0) & (out["efp_prev_downtrend"] == 0.0))
    assert one_bar.any()
    assert set(out.loc[one_bar, "efp_reason"]) == {"trend_unconfirmed"}
    assert (out.loc[one_bar, "signal"] == 0).all()


INVALID_PARAMS = [
    ({"force_period": 1}, "force_period"),
    ({"force_period": 13.5}, "force_period"),
    ({"force_period": True}, "force_period"),
    ({"force_period": float("nan")}, "force_period"),
    ({"force_period": float("inf")}, "force_period"),
    ({"force_period": "13"}, "force_period"),
    ({"trend_period": 1}, "trend_period"),
    ({"trend_period": False}, "trend_period"),
    ({"slope_span": 0}, "slope_span"),
    ({"recursion_length": 129}, "recursion_length"),
    ({"recursion_length": 198}, "recursion_length"),
    ({"force_period": 20, "recursion_length": 197}, "recursion_length"),
    ({"volume_weighting": 2}, "volume_weighting"),
    ({"volume_weighting": True}, "volume_weighting"),
    ({"volume_weighting": 0.5}, "volume_weighting"),
    ({"trend_period": 190, "slope_span": 9}, "required history"),
    ({"unknown_knob": 1}, "unknown parameters"),
]


def test_invalid_parameters_are_refused_by_the_registry_and_the_evaluator():
    for override, needle in INVALID_PARAMS:
        with _case(f"{override}-{needle}"):
            with pytest.raises(ValueError, match=needle):
                _reg()._registry.validate_params(NAME, override)
            if "unknown_knob" not in override:
                with pytest.raises(ValueError):
                    _apply(_frame(_pullback()), **override)


def test_the_largest_allowed_history_is_199_rows():
    for over in ({"recursion_length": 197}, {"trend_period": 190, "slope_span": 8},
                 {"force_period": 19, "recursion_length": 190}):
        _reg()._registry.validate_params(NAME, over)
    assert _apply(_frame(_pullback()), recursion_length=197)["efp_required_history"].iloc[0] == 199.0
    assert _apply(_frame(_pullback()), trend_period=190, slope_span=8)["efp_required_history"].iloc[0] == 199.0


DEFECT_AT = CROSS_AT - 40
DEFECT_REASONS = {"nonfinite_input", "nonpositive_price", "invalid_candle_geometry", "negative_volume",
                  "timestamp_order", "cadence_gap", "missing_timestamps"}


WINDOW_DEFECTS = [
    (lambda df: _set(df, DEFECT_AT, "close", np.nan), "nonfinite_input", 0),
    (lambda df: _set(df, DEFECT_AT, "volume", np.inf), "nonfinite_input", 0),
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
    for i, (mutate, reason, at) in enumerate(WINDOW_DEFECTS):
        with _case(f"{i}-{reason}-{at}"):
            _defects_inside_the_required_window_hold_and_recover_after_a_clean_window(mutate, reason, at)


def _defects_inside_the_required_window_hold_and_recover_after_a_clean_window(mutate, reason, at):
    clean = _frame(_pullback(n=420))
    assert int(_apply(clean)["signal"].iloc[CROSS_AT]) == 1
    out = _apply(mutate(clean))
    assert out["efp_reason"].iloc[DEFECT_AT + at] == reason
    assert out["efp_reason"].iloc[CROSS_AT] in DEFECT_REASONS and int(out["signal"].iloc[CROSS_AT]) == 0
    assert out["efp_valid"].iloc[CROSS_AT] == 0.0 and np.isnan(out["efp_force"].iloc[CROSS_AT])
    held = out.iloc[DEFECT_AT:DEFECT_AT + NEED - 1]
    assert held["efp_valid"].eq(0.0).all()
    assert out["efp_valid"].iloc[DEFECT_AT + NEED + 2] == 1.0


def test_received_order_defects_hold_like_a_sort_repair():
    clean = _frame(_pullback())
    flagged = _copy(clean)
    flagged.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(clean.index[DEFECT_AT].value // 1_000_000)]
    out = _apply(flagged)
    assert out["efp_reason"].iloc[CROSS_AT] == "timestamp_order" and int(out["signal"].abs().sum()) == 0


def test_defects_outside_the_required_window_never_change_the_decision():
    clean = _frame(_pullback())
    base = _apply(clean)
    first = CROSS_AT - NEED + 1
    for pos in (0, 10, first - 1):
        out = _apply(_set(clean, pos, "close", np.nan))
        for col in EVENT_COLS:
            assert _same(out[col].iloc[CROSS_AT], base[col].iloc[CROSS_AT]), (pos, col)
    gap = _with_index(clean, list(clean.index[:first]) + list(clean.index[first:] + pd.Timedelta(hours=3)))
    out = _apply(gap)
    assert out["efp_valid"].iloc[CROSS_AT] == 1.0 and int(out["signal"].iloc[CROSS_AT]) == 1
    edge = _with_index(clean, list(clean.index[:first + 1]) + list(clean.index[first + 1:] + pd.Timedelta(hours=3)))
    assert _apply(edge)["efp_reason"].iloc[CROSS_AT] == "cadence_gap"


def test_insufficient_history_and_frame_level_holds():
    df = _frame(_pullback())
    tail = df.iloc[CROSS_AT - NEED + 1:CROSS_AT + 1].copy()
    tail.attrs = dict(df.attrs)
    assert int(_apply(tail)["signal"].iloc[-1]) == 1
    short = tail.iloc[1:].copy()
    short.attrs = dict(df.attrs)
    got = _apply(short)
    assert set(got["efp_reason"]) == {"insufficient_history"} and int(got["signal"].abs().sum()) == 0
    undeclared = df.copy()
    undeclared.attrs = {}
    assert set(_apply(undeclared)["efp_reason"]) == {"missing_cadence"}
    bad = df.copy()
    bad.attrs = {FRAME_INTERVAL_ATTR: True}
    assert set(_apply(bad)["efp_reason"]) == {"missing_cadence"}
    assert set(_apply(df.drop(columns=["volume"]))["efp_reason"]) == {"missing_columns"}
    assert set(_apply(df.reset_index(drop=True))["efp_reason"]) == {"missing_timestamps"}


def test_derived_overflow_holds_and_recovers():
    df = _frame(_pullback(n=420))
    huge = _set(df, FIRST_PULLBACK, "volume", 1e308)
    out = _apply(huge)
    assert np.isinf((huge["close"].iloc[FIRST_PULLBACK] - huge["close"].iloc[FIRST_PULLBACK - 1]) * 1e308)
    assert out["efp_reason"].iloc[FIRST_PULLBACK] == "nonfinite_derived"
    assert out["efp_reason"].iloc[CROSS_AT] == "nonfinite_derived" and int(out["signal"].abs().sum()) == 0
    first_clean = FIRST_PULLBACK + SEED["recursion_length"] + 1
    assert out["efp_valid"].iloc[first_clean - 1] == 0.0 and out["efp_valid"].iloc[first_clean] == 1.0
    assert _apply(_frame(_pullback(n=420)))["efp_valid"].iloc[FIRST_PULLBACK] == 1.0


def test_native_close_and_timezone_inputs_decide_the_same():
    df = _frame(_pullback())
    base = _apply(df)
    native = _copy(df)
    native.index = native.index + pd.Timedelta(milliseconds=3_600_000 - 1)
    native.attrs[FRAME_TIMESTAMP_KIND_ATTR] = TIMESTAMP_KIND_NATIVE_CLOSE
    ny = _copy(df)
    ny.index = ny.index.tz_localize("UTC").tz_convert("America/New_York")
    for variant in (native, ny):
        got = _apply(variant)
        for col in EVENT_COLS:
            assert all(_same(a, b) for a, b in zip(got[col], base[col])), col


def test_full_series_every_prefix_and_bounded_tails_agree():
    df = _walk(520, seed=11)
    for flag in (1, 0):
        full = _apply(df, volume_weighting=flag)
        valid = full["efp_valid"].to_numpy() == 1.0
        assert (full["signal"] != 0).any()
        for i in range(len(df)):
            frames = [df.iloc[:i + 1]]
            if valid[i]:
                frames += [df.iloc[i + 1 - size:i + 1] for size in (NEED, 199, 200) if i + 1 >= size]
            for frame in frames:
                frame = _copy(frame)
                frame.attrs = dict(df.attrs)
                got = _apply(frame, volume_weighting=flag).iloc[-1]
                for col in EVENT_COLS:
                    assert _same(got[col], full[col].iloc[i]), (flag, i, len(frame), col)


def test_finite_average_stays_within_the_frozen_sensitivity_tolerance():
    study = _study()
    df = _walk(900, seed=21)
    df["timestamp"] = df.index.as_unit("ms").asi8
    df = declare_frame_cadence(df, "1h", TIMESTAMP_KIND_OPEN)
    for flag in (1, 0):
        cell = study.sensitivity_cell(_reg(), df, dict(SEED, volume_weighting=flag))
        assert cell["valid_bars"] > 0 and cell["within_tolerance"], cell
        assert cell["max_normalized_difference"] <= 1e-8


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
        preflight_reference_params(open_refs, {"volume_weighting": True}, {}, reg.get_strategy, reg.validate_params)


def test_research_legs_refuse_invalid_parameters_before_loading_candles(monkeypatch):
    import data_fetcher

    def refuse(*_a, **_k):
        raise AssertionError("candles were loaded before the parameters were validated")

    monkeypatch.setattr(data_fetcher, "load_cached_data", refuse)
    with pytest.raises(ValueError, match="recursion_length"):
        eval_windows.run_leg(_reg(), NAME, {"recursion_length": 40}, "BTC/USDT", "4h", ("2026-01-01", "2026-02-01"))


def _composer(df, side="", position_ctx=None, protection_df=None, params=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side,
        params=dict(params or SEED), position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": CLOSE[0]["params"]}, protection_df=protection_df)
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    long_df = _frame(_pullback(1)).iloc[:CROSS_AT + 1].copy()
    short_df = _frame(_pullback(-1)).iloc[:CROSS_AT + 1].copy()
    _, lg = _composer(long_df)
    _, s = _composer(short_df)
    assert (lg["open_action"], lg["signal"]) == ("long", 1)
    assert (s["open_action"], s["signal"]) == ("short", -1)
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
    df = _frame(_pullback(1)).iloc[:CROSS_AT + 1].copy()
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
        assert evaluation.open_result_df["efp_valid"].iloc[-1] == 0.0
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
    df = _walk(3000, seed=31)
    sig = ensure_atr_indicator(_apply(df))
    assert (sig["signal"] == 1).sum() >= 3 and (sig["signal"] == -1).sum() >= 3
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"}
    positions = {}
    for t in trades:
        entry = pd.Timestamp(t["entry_date"])
        pos = sig.index.get_loc(entry)
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        side, last_exit = positions.get(entry, (t["side"], pd.Timestamp(t["exit_date"])))
        assert side == t["side"]
        positions[entry] = (side, max(last_exit, pd.Timestamp(t["exit_date"])))
    assert any(t["exit_reason"].startswith("tiered_tp_pct") for t in trades)
    spans = sorted((entry, exit_) for entry, (_, exit_) in positions.items())
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
    df = _walk(700, seed=41)
    frame = compute_parity_frame(df, cfg=_cfg(batched=batched), window=window or None)
    result = summarize(frame)
    assert result["bars_compared"] > 0 and result["mismatches"] == 0, frame[~frame["match"]].head()
    assert result["clean"] and result["close_validation"]["close_eligibility"] == "eligible"
    if batched:
        assert result["batch_mismatches"] == 0
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()


WF_SPLITS = 4
WF_RANGES = {"force_period": [13], "trend_period": [50], "slope_span": [5], "recursion_length": [150, 197],
             "volume_weighting": [1]}


def _walk_forward_capture(monkeypatch, df):
    runs = []
    real_run = Backtester.run

    def run(self, signals, **kw):
        if kw.get("warmup_prefix") is not None:
            raise AssertionError("the walk-forward warmup seeder ran for elder_force_pullback")
        runs.append({"signals": signals.copy(), "params": dict(kw["params"]), "seed": kw.get("starting_long")})
        return real_run(self, signals, **kw)

    monkeypatch.setattr(Backtester, "run", run)
    result = optimizer.walk_forward_optimize(
        df, NAME, WF_RANGES, n_splits=WF_SPLITS, registry="futures", timeframe="1h", platform="hyperliquid",
        verbose=False, close_strategies=CLOSE, direction="both", stop_loss_atr_mult=1.0,
        comparison_mode="approximate")
    monkeypatch.undo()
    return result, runs


def _same_column(a, b):
    return list(a.index) == list(b.index) and [repr(v) for v in a] == [repr(v) for v in b]


def _fold_geometry(df, fold):
    window = len(df) // WF_SPLITS
    start = fold * window
    end = min(start + window, len(df)) if fold < WF_SPLITS - 1 else len(df)
    return start, start + int((end - start) * 0.7)


def _train_run(runs, df, bar, recursion_length):
    return next(r for r in runs if r["signals"].index[0] == df.index[bar]
                and r["params"]["recursion_length"] == recursion_length)


def test_walk_forward_warmup_covers_the_largest_required_history_of_the_grid():
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert max(p["recursion_length"] for p in grid) == 197
    assert strategy_warmup_bars(NAME, grid, DEFAULT_PARAM_RANGES[NAME]) == 199
    assert strategy_warmup_bars(NAME, generate_param_grid(dict(WF_RANGES, recursion_length=[150])),
                                dict(WF_RANGES, recursion_length=[150])) == NEED


def test_walk_forward_scores_the_first_bar_of_a_later_fold_with_full_history(monkeypatch):
    df = _walk(2400, seed=0).iloc[111:]
    start, boundary = _fold_geometry(df, 1)
    deep = dict(SEED, recursion_length=197)
    full_deep = _reg().apply_strategy(NAME, df, deep)
    assert int(full_deep["signal"].iloc[start - 1]) == 1
    short = _reg().apply_strategy(NAME, df.iloc[start - 197:start], deep)
    assert short["efp_reason"].iloc[-1] == "insufficient_history"
    result, runs = _walk_forward_capture(monkeypatch, df)
    assert result["n_valid_folds"] >= 2
    first = _train_run(runs, df, start - 1, 197)["signals"]
    assert int(first["signal"].iloc[0]) == 1 and first["efp_reason"].iloc[0] != "insufficient_history"
    shallow = _train_run(runs, df, start - 1, 150)["signals"]
    full_shallow = _apply(df).iloc[start - 1:boundary]
    old_prefix = _apply(df.iloc[start - 197:boundary]).iloc[196:]
    for col in ("signal", "efp_reason", "efp_force"):
        assert _same_column(shallow[col], full_shallow[col]), col
        assert _same_column(shallow[col], old_prefix[col]), col
    _, first_boundary = _fold_geometry(df, 0)
    fold0 = _train_run(runs, df, 0, 150)["signals"]
    assert len(fold0) == first_boundary
    for col in ("signal", "efp_reason"):
        assert _same_column(fold0[col], _apply(df.iloc[:first_boundary])[col]), col


@pytest.mark.parametrize("trim,case", [(111, "long_reaches_the_last_tier"), (250, "long_then_short")])
def test_walk_forward_folds_start_flat_whatever_the_prefix_left_open(monkeypatch, trim, case):
    df = _walk(2400, seed=0).iloc[trim:]
    start, _ = _fold_geometry(df, 1)
    warmup = strategy_warmup_bars(NAME, generate_param_grid(WF_RANGES), WF_RANGES)
    prefix = _apply(df.iloc[start - warmup:start]).iloc[:warmup - 1]
    if case == "long_reaches_the_last_tier":
        seeded = warmup_exit_long_entry(prefix, 0.0)
        assert seeded is not None
        assert prefix.loc[seeded["entry_date"]:, "close"].max() >= seeded["entry_price"] * (1 + TIERS[-1]["profit_pct"])
    else:
        assert [int(v) for v in prefix["signal"] if v != 0][-2:] == [1, -1]
    _, runs = _walk_forward_capture(monkeypatch, df)
    assert all(r["seed"] is None for r in runs)
    assert _train_run(runs, df, start - 1, 150)["seed"] is None


def _load(name):
    path = os.path.join(STUDY_DIR, f"{name}.py")
    spec = importlib.util.spec_from_file_location(f"_efp_1668_{name}", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _study():
    return _load("run_study")


def test_study_protocol_lock_grid_and_verdict_rules():
    study = _study()
    seed = json.load(open(os.path.join(STUDY_DIR, "candidate_seed.json")))
    assert seed["params"] == SEED and seed["optimizer_grid"] == {k: list(v) for k, v in DEFAULT_PARAM_RANGES[NAME].items()}
    assert seed["close_strategies"] == CLOSE and seed["incumbent_contract"]["close_strategies"] == CLOSE
    assert study.verify_lock()["candidate_seed_sha256"] == study._sha(study.SEED_PATH)
    assert study.check_grid(_reg())["all_valid"]
    assert study.ABLATION == "elder_force_pullback_vw0" and study.paired_params(study.ABLATION, SEED)[
        "volume_weighting"] == 0
    assert set(study.COMPARATORS) == {"elder_force_pullback_vw0", "momentum_pro", "elder_ray_pullback"}

    def arm(ip, sharpe=1.0, ddadj=1.0, net=10.0, longs=5, shorts=5):
        block = {"independent_positions": ip, "mean_sharpe": sharpe, "mean_ddadj": ddadj, "net_pnl_usd": net,
                 "long_positions": longs, "short_positions": shorts, "coverage_complete": True}
        return {"selected": {"base": block, "stress": dict(block)}}

    gate = {"complete": True, "refused": []}
    held = {n: arm(40, sharpe=0.5, ddadj=0.5) for n in study.ARMS}
    held[study.CANDIDATE] = arm(40)
    assert study.verdict(gate, {}, held)["outcome"] == "pass"
    assert study.verdict(gate, {}, held)["edge_source"] == "unvalidated"
    held[study.CANDIDATE] = arm(40, sharpe=0.4)
    assert study.verdict(gate, {}, held)["edge_source"] == "study_fail"
    held[study.CANDIDATE] = arm(20)
    out = study.verdict(gate, {}, held)
    assert out["outcome"] == "inconclusive" and out["edge_source"] == "study_inconclusive"
    held[study.CANDIDATE] = arm(40, shorts=0)
    assert study.verdict(gate, {}, held)["outcome"] == "inconclusive"
    held[study.CANDIDATE] = arm(40)
    held["momentum_pro"] = {"selected": None}
    assert study.verdict(gate, {}, held)["outcome"] == "inconclusive"
    assert study.verdict({"complete": False, "refused": [{"dataset": "BTC 4h", "window": "test"}]}, {}, {})[
        "outcome"] == "inconclusive"
