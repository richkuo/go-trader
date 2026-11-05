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

from closed_bar import (
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "mass_index_reversal"
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


def _check_upper_crossing_arms_and_the_first_lower_crossing_completes_once():
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


def _check_rising_and_falling_slopes_give_mirrored_entries_on_the_completion_bar():
    short = _apply(_frame(*_short_entry_mid(0.05)))
    assert int(short["signal"].iloc[DOWN_AT]) == -1 and short["mir_reason"].iloc[DOWN_AT] == "entry_short"
    assert short["mir_bulge_direction"].iloc[DOWN_AT] == -1.0 and short["mir_bulge_slope"].iloc[DOWN_AT] > 0
    assert int(short["signal"].abs().sum()) == 1
    long_ = _apply(_frame(*_short_entry_mid(-0.05)))
    assert int(long_["signal"].iloc[DOWN_AT]) == 1 and long_["mir_reason"].iloc[DOWN_AT] == "entry_long"
    assert long_["mir_bulge_direction"].iloc[DOWN_AT] == 1.0 and long_["mir_bulge_slope"].iloc[DOWN_AT] < 0
    assert int(long_["signal"].abs().sum()) == 1


def _check_direction_is_frozen_at_the_upper_crossing():
    rng, mid = _burst(slope=0.05)
    mid = mid.copy()
    mid[UP_AT + 1:] = mid[UP_AT] - 2.0 * np.arange(1, len(mid) - UP_AT)
    out = _apply(_frame(rng, mid))
    assert out["mir_trend_slope"].iloc[DOWN_AT] < 0 and out["mir_bulge_slope"].iloc[DOWN_AT] > 0
    assert int(out["signal"].iloc[DOWN_AT]) == -1


def _check_price_confirmation_is_strict_against_the_previous_highs_and_lows():
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


def _check_filter_off_tests_every_bar_with_the_current_slope():
    df = _walk(480, seed=9)
    off = _apply(df, bulge_filter=0)
    on = _apply(df)
    valid = off["mir_valid"] == 1.0
    slope = off["mir_trend_slope"][valid]
    side = off["mir_tested_side"][valid]
    assert ((slope < 0) == (side == 1)).all() and ((slope > 0) == (side == -1)).all()
    assert (off["signal"] != 0).sum() > (on["signal"] != 0).sum()
    assert set(off["mir_reason"][valid]) <= {"entry_long", "entry_short", "confirmation_failed", "zero_slope"}


def test_entry_rule_cases_arm_complete_and_hold_with_their_reasons(subtests):
    for label, check in (
        ("upper_crossing_arms_and_the_first_lower_crossing_completes_once", _check_upper_crossing_arms_and_the_first_lower_crossing_completes_once),
        ("rising_and_falling_slopes_give_mirrored_entries_on_the_completion_bar", _check_rising_and_falling_slopes_give_mirrored_entries_on_the_completion_bar),
        ("direction_is_frozen_at_the_upper_crossing", _check_direction_is_frozen_at_the_upper_crossing),
        ("price_confirmation_is_strict_against_the_previous_highs_and_lows", _check_price_confirmation_is_strict_against_the_previous_highs_and_lows),
        ("filter_off_tests_every_bar_with_the_current_slope", _check_filter_off_tests_every_bar_with_the_current_slope),
    ):
        with subtests.test(case=label):
            check()


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


def _check_defects_outside_the_required_window_never_change_the_decision():
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


def test_decisions_depend_only_on_the_required_window(subtests):
    for label, check in (
        ("defects_outside_the_required_window_never_change_the_decision", _check_defects_outside_the_required_window_never_change_the_decision),
    ):
        with subtests.test(case=label):
            check()


def _cfg(**kw):
    base = dict(strategy_name=NAME, params=dict(SEED), registry="futures", platform="hyperliquid", symbol="BTC",
                timeframe="1h", close_refs=json.loads(json.dumps(CLOSE)), direction="both",
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict")
    base.update(kw)
    return ParityConfig(**base)


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
