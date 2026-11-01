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
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    bind_fixed_interval_params,
    declare_frame_cadence,
)
from parity_diff import LIVE_MIN_CANDLES, ParityConfig
from registry_loader import load_registry
from strategy_composition import (
    StrategyReference,
    preflight_reference_params,
)

NAME = "camarilla_pivot_reversal"
STUDY = os.path.join(_REPO, "backtest", "candidates", "camarilla_pivot_reversal_1669", "run_study.py")
DAY = 86_400_000
H = 3_600_000
SUPPORTED = {"15m": 900_000, "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000, "4h": 14_400_000,
             "8h": 28_800_000, "12h": 43_200_000}
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
S3, R3, S4, R4 = 94.5, 105.5, 89.0, 111.0


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


def _apply(df, interval=H):
    return _reg().apply_strategy(NAME, df, {"expected_interval_ms": interval})


def _bar(o, c, h=None, lo=None):
    return (float(o), float(max(o, c) + 0.1 if h is None else h), float(min(o, c) - 0.1 if lo is None else lo),
            float(c))


def _prior_day(per, high=110.0, low=90.0, close=100.0):
    bars = [_bar(100.0, 100.0) for _ in range(per)]
    bars[0] = (100.0, high, 99.9, 100.0)
    bars[-1] = (100.0, max(100.0, close) + 0.1, low, close)
    return bars


def _session(closes, start=100.0):
    out, prev = [], start
    for c in closes:
        out.append(_bar(prev, c))
        prev = c
    return out


def _frame(bars, tf="1h", start="2026-03-02", kind=TIMESTAMP_KIND_OPEN):
    step = SUPPORTED[tf]
    idx = pd.date_range(start, periods=len(bars), freq=pd.Timedelta(milliseconds=step))
    o, h, lo, c = (list(x) for x in zip(*bars))
    df = pd.DataFrame({"open": o, "high": h, "low": lo, "close": c, "volume": 1000.0}, index=idx)
    df.index.name = "datetime"
    return declare_frame_cadence(df, tf, kind)


def _day_frame(tf, current, prior=None):
    per = DAY // SUPPORTED[tf]
    bars = _prior_day(per) if prior is None else prior
    current = current + [100.0] * (per - len(current))
    return _frame(bars + _session(current), tf)


def _walk(n, tf, seed, start="2026-03-01", sigma=None):
    rng = np.random.default_rng(seed)
    step = SUPPORTED[tf]
    sig = sigma if sigma is not None else 0.006 * (step / H) ** 0.5
    close = 100.0 * np.exp(np.cumsum(rng.normal(0.0, sig, n)))
    open_ = np.concatenate([[100.0], close[:-1]])
    wick = np.abs(rng.normal(0.0, sig / 2, n))
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) * (1 + wick),
                       "low": np.minimum(open_, close) * (1 - wick), "close": close,
                       "volume": rng.uniform(100.0, 1000.0, n)},
                      index=pd.date_range(start, periods=n, freq=pd.Timedelta(milliseconds=step)))
    df.index.name = "datetime"
    return declare_frame_cadence(df, tf, TIMESTAMP_KIND_OPEN)


def _reference(df, interval):
    per = DAY // interval
    ms = np.asarray([int(t.value // 1_000_000) for t in df.index], dtype=np.int64)
    days = ms // DAY
    rows = {int(t): i for i, t in enumerate(ms)}
    out = []
    for i in range(len(df)):
        expect = {"signal": 0, "valid": False}
        if i >= 1 and ms[i] - ms[i - 1] == interval and days[i] == days[i - 1]:
            start = (days[i] - 1) * DAY
            idx = [rows.get(start + k * interval) for k in range(per)]
            if all(j is not None for j in idx):
                prior = df.iloc[idx]
                hi, lo, cl = float(prior["high"].max()), float(prior["low"].min()), float(prior["close"].iloc[-1])
                rng = hi - lo
                if rng > 0:
                    s3, r3 = cl - 1.1 * rng / 4, cl + 1.1 * rng / 4
                    s4, r4 = cl - 1.1 * rng / 2, cl + 1.1 * rng / 2
                    pc, c = float(df["close"].iloc[i - 1]), float(df["close"].iloc[i])
                    pl, low = float(df["low"].iloc[i - 1]), float(df["low"].iloc[i])
                    ph, high = float(df["high"].iloc[i - 1]), float(df["high"].iloc[i])
                    sig = 0
                    if pc <= s3 < c < r3 and pl > s4 and low > s4:
                        sig = 1
                    elif pc >= r3 > c > s3 and ph < r4 and high < r4:
                        sig = -1
                    expect = {"signal": sig, "valid": True, "s3": s3, "r3": r3, "s4": s4, "r4": r4, "H": hi,
                              "L": lo, "C": cl}
        out.append(expect)
    return out


def test_levels_and_entries_match_an_independent_daily_calculation(subtests):
    for tf in SUPPORTED:
        with subtests.test(tf=tf):
            _levels_and_entries_match_an_independent_daily_calculation(tf)


def _levels_and_entries_match_an_independent_daily_calculation(tf):
    interval = SUPPORTED[tf]
    per = DAY // interval
    df = _walk(per * 9, tf, seed=interval // 60_000, sigma=0.02 / per ** 0.5)
    out = _apply(df, interval)
    ref = _reference(df, interval)
    for i, want in enumerate(ref):
        assert int(out["signal"].iloc[i]) == want["signal"], (tf, i, out["cpr_reason"].iloc[i])
        assert (out["cpr_valid"].iloc[i] == 1.0) == want["valid"], (tf, i, out["cpr_reason"].iloc[i])
        if want["valid"]:
            for key, col in (("s3", "cpr_s3"), ("r3", "cpr_r3"), ("s4", "cpr_s4"), ("r4", "cpr_r4"),
                             ("H", "cpr_prior_high"), ("L", "cpr_prior_low"), ("C", "cpr_prior_close")):
                assert out[col].iloc[i] == pytest.approx(want[key], rel=1e-12, abs=1e-12)
    assert out["cpr_valid"].iloc[:per].eq(0.0).all()
    assert (out["cpr_reason"].iloc[:per] == "insufficient_history").all()


def test_designed_days_emit_both_entries_with_their_evaluated_inputs():
    long_out = _apply(_day_frame("1h", [100.0, 94.0, 95.0]))
    i = 24 + 2
    assert int(long_out["signal"].iloc[i]) == 1 and long_out["cpr_reason"].iloc[i] == "entry_long"
    rec = long_out.iloc[i]
    assert (rec["cpr_prior_high"], rec["cpr_prior_low"], rec["cpr_prior_close"]) == (110.0, 90.0, 100.0)
    assert rec["cpr_s3"] == pytest.approx(S3) and rec["cpr_r3"] == pytest.approx(R3)
    assert rec["cpr_s4"] == pytest.approx(S4) and rec["cpr_r4"] == pytest.approx(R4)
    assert (rec["cpr_prev_close"], rec["cpr_close"]) == (94.0, 95.0)
    assert rec["cpr_eval_ts_ms"] == rec["cpr_day_open_ms"] + 2 * H
    assert rec["cpr_prior_day_open_ms"] == rec["cpr_day_open_ms"] - DAY and rec["cpr_interval_ms"] == H
    assert int(long_out["signal"].abs().sum()) == 1
    short_out = _apply(_day_frame("1h", [100.0, 106.0, 105.0]))
    assert int(short_out["signal"].iloc[i]) == -1 and short_out["cpr_reason"].iloc[i] == "entry_short"
    assert int(short_out["signal"].abs().sum()) == 1


LEVEL_PATHS = [
    ([100.0, S3, 95.0], "entry_long", 1),
    ([100.0, 94.0, S3], "level_equal", 0),
    ([100.0, 94.0, R3], "long_opposite_jump", 0),
    ([100.0, 94.0, 107.0], "long_opposite_jump", 0),
    ([100.0, R3, 105.0], "entry_short", -1),
    ([100.0, 106.0, R3], "level_equal", 0),
    ([100.0, 106.0, S3], "short_opposite_jump", 0),
    ([100.0, 106.0, 93.0], "short_opposite_jump", 0),
    ([100.0, 96.0, 97.0], "no_cross", 0),
]


def _level_equality_and_opposite_boundary_jumps():
    for path, reason, signal in LEVEL_PATHS:
        with _case(path):
            out = _apply(_day_frame("1h", path))
            assert out["cpr_reason"].iloc[26] == reason and int(out["signal"].iloc[26]) == signal


OUTER_SUPPORT_CASES = [
    (S4, 93.9, "long_outer_breach"),
    (89.5, S4, "long_outer_breach"),
    (88.0, 93.9, "long_outer_breach"),
    (S4 + 1e-9, 93.9, "entry_long"),
]


def _outer_support_contact_invalidates_a_long():
    for prev_low, low, reason in OUTER_SUPPORT_CASES:
        with _case((prev_low, low, reason)):
            df = _day_frame("1h", [100.0, 94.0, 95.0])
            df.iloc[25, df.columns.get_loc("low")] = prev_low
            df.iloc[26, df.columns.get_loc("low")] = low
            assert _apply(df)["cpr_reason"].iloc[26] == reason


OUTER_RESISTANCE_CASES = [
    (R4, 106.1, "short_outer_breach"),
    (106.2, R4, "short_outer_breach"),
    (R4 - 1e-9, 106.1, "entry_short"),
]


def _outer_resistance_contact_invalidates_a_short():
    for prev_high, high, reason in OUTER_RESISTANCE_CASES:
        with _case((prev_high, high, reason)):
            df = _day_frame("1h", [100.0, 106.0, 105.0])
            df.iloc[25, df.columns.get_loc("high")] = prev_high
            df.iloc[26, df.columns.get_loc("high")] = high
            assert _apply(df)["cpr_reason"].iloc[26] == reason


def test_entry_levels_decide_by_strict_crossings_and_outer_contacts(subtests):
    with subtests.test(case="level_equality_and_opposite_boundary_jumps"):
        _level_equality_and_opposite_boundary_jumps()
    with subtests.test(case="outer_support_contact_invalidates_a_long"):
        _outer_support_contact_invalidates_a_long()
    with subtests.test(case="outer_resistance_contact_invalidates_a_short"):
        _outer_resistance_contact_invalidates_a_short()


def test_the_first_bar_of_a_day_never_pairs_with_the_previous_day():
    prior = _prior_day(24)
    prior[-1] = (100.0, 100.1, 90.0, 93.0)
    df = _frame(prior + _session([95.0] + [100.0] * 23, start=93.0))
    out = _apply(df)
    assert out["cpr_reason"].iloc[24] == "first_bar_of_day" and int(out["signal"].iloc[24]) == 0
    assert out["cpr_prior_close"].iloc[25] == 93.0


def test_rising_falling_and_constant_inputs():
    for shape in ("rising", "falling", "constant"):
        with _case(shape):
            _rising_falling_and_constant_inputs(shape)


def _rising_falling_and_constant_inputs(shape):
    n = 24 * 4
    if shape == "rising":
        closes = list(100.0 + 0.1 * np.arange(n))
    elif shape == "falling":
        closes = list(100.0 - 0.1 * np.arange(n))
    else:
        closes = [100.0] * n
    bars = [(c, c, c, c) for c in closes] if shape == "constant" else _session(closes, start=closes[0])
    df = _frame(bars)
    out = _apply(df)
    ref = _reference(df, H)
    assert [int(s) for s in out["signal"]] == [r["signal"] for r in ref]
    later = out.iloc[25:]
    if shape == "constant":
        assert set(later["cpr_reason"]) == {"zero_range", "first_bar_of_day"}
        assert later["cpr_valid"].eq(0.0).all()
    else:
        assert later.loc[later["cpr_reason"] != "first_bar_of_day", "cpr_valid"].eq(1.0).all()
        assert int(out["signal"].abs().sum()) == 0


def _with_index(df, idx):
    out = df.copy()
    out.index = pd.DatetimeIndex(idx, name="datetime")
    out.attrs = dict(df.attrs)
    return out


def _drop(df, pos):
    out = df.drop(df.index[pos])
    out.attrs = dict(df.attrs)
    return out


def _set(df, pos, col, value):
    out = df.copy()
    out.iloc[pos, out.columns.get_loc(col)] = value
    return out


PRIOR_DAY_DEFECTS = [
    (lambda df: _drop(df, 10), "prior_day_incomplete"),
    (lambda df: _drop(df, 23), "prior_day_incomplete"),
    (lambda df: _drop(df, 0), "insufficient_history"),
    (lambda df: _set(df, 7, "close", np.nan), "nonfinite_price"),
    (lambda df: _set(df, 7, "high", np.inf), "nonfinite_price"),
    (lambda df: _set(df, 7, "low", 0.0), "nonpositive_price"),
    (lambda df: _set(df, 7, "low", -1.0), "nonpositive_price"),
    (lambda df: _set(df, 7, "open", 200.0), "inconsistent_candle"),
    (lambda df: _set(df, 7, "high", 50.0), "inconsistent_candle"),
    (lambda df: _with_index(df, list(df.index[:8]) + [df.index[7]] + list(df.index[9:])), "duplicate_timestamp"),
    (lambda df: _with_index(df, list(df.index[:7]) + [df.index[8], df.index[7]] + list(df.index[9:])),
     "descending_timestamp"),
    (lambda df: _with_index(df, list(df.index[:7]) + [df.index[7] + pd.Timedelta(minutes=30)]
                            + list(df.index[8:])), "grid_misaligned"),
    (lambda df: _with_index(df, list(df.index[:7]) + [pd.NaT] + list(df.index[8:])), "missing_timestamp"),
]


def _prior_day_defects_hold_every_decision_of_the_next_day():
    for i, (mutate, reason) in enumerate(PRIOR_DAY_DEFECTS):
        with _case(f"{i}-{reason}"):
            clean = _day_frame("1h", [100.0, 94.0, 95.0])
            out = _apply(mutate(clean))
            cur = out.iloc[-24:]
            assert int(cur["signal"].abs().sum()) == 0 and cur["cpr_valid"].eq(0.0).all()
            pairs = cur.iloc[1:]
            assert set(pairs["cpr_reason"]) == {reason}, set(pairs["cpr_reason"])


def test_received_out_of_order_evidence_survives_the_callers_sort():
    clean = _day_frame("1h", [100.0, 94.0, 95.0])
    flagged = clean.copy()
    flagged.attrs = dict(clean.attrs)
    flagged.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(clean.index[7].value // 1_000_000)]
    out = _apply(flagged)
    assert out["cpr_reason"].iloc[7] == "received_out_of_order"
    assert set(out["cpr_reason"].iloc[25:]) == {"received_out_of_order"}
    assert int(out["signal"].abs().sum()) == 0
    late = clean.copy()
    late.attrs = dict(clean.attrs)
    late.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(clean.index[30].value // 1_000_000)]
    out = _apply(late)
    assert int(out["signal"].iloc[26]) == 1 and out["cpr_reason"].iloc[30] == "received_out_of_order"


def test_a_gap_inside_the_current_day_resets_the_pair_and_entries_resume_after_it():
    df = _day_frame("1h", [100.0, 100.0, 100.0, 100.0, 94.0, 95.0])
    out = _apply(_drop(df, 24 + 2))
    gap_row = 24 + 2
    assert out["cpr_reason"].iloc[gap_row] == "cadence_gap" and int(out["signal"].iloc[gap_row]) == 0
    assert int(out["signal"].iloc[24 + 4]) == 1
    straddle = _apply(_drop(_day_frame("1h", [100.0, 94.0, 100.0, 95.0]), 24 + 2))
    assert straddle["cpr_reason"].iloc[24 + 2] == "cadence_gap" and int(straddle["signal"].abs().sum()) == 0


def _both_crossing_candles_are_validated():
    for pos, reason in ((25, "nonfinite_price"), (26, "nonfinite_price")):
        with _case(pos):
            out = _apply(_set(_day_frame("1h", [100.0, 94.0, 95.0]), pos, "close", np.nan))
            assert out["cpr_reason"].iloc[26] == reason and int(out["signal"].abs().sum()) == 0


def _levels_never_carry_over_a_missing_day():
    per = 24
    bars = _prior_day(per) + _session([100.0] * per) + _session([100.0, 94.0, 95.0] + [100.0] * (per - 3))
    df = _frame(bars)
    keep = [i for i in range(len(df)) if not (per <= i < 2 * per)]
    gapped = df.iloc[keep].copy()
    gapped.attrs = dict(df.attrs)
    out = _apply(gapped)
    cur = out.iloc[per:]
    assert set(cur["cpr_reason"].iloc[1:]) == {"prior_day_missing"}
    assert int(out["signal"].abs().sum()) == 0 and cur["cpr_s3"].isna().all()


def test_later_rows_never_change_an_earlier_decision():
    df = _walk(24 * 12, "1h", seed=5, sigma=0.01)
    full = _apply(df)
    cols = ["signal", "cpr_reason", "cpr_valid", "cpr_s3", "cpr_r3", "cpr_s4", "cpr_r4"]
    for cut in (60, 130, 200, 250):
        damaged = df.copy()
        damaged.iloc[cut + 1:, damaged.columns.get_loc("close")] = np.nan
        damaged.attrs = dict(df.attrs)
        damaged.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[cut + 2].value // 1_000_000)]
        got = _apply(damaged)
        pd.testing.assert_frame_equal(got[cols].iloc[:cut + 1], full[cols].iloc[:cut + 1])


def test_full_series_expanding_prefix_and_200_row_tail_agree_including_early_rows():
    for tf in ("15m", "1h", "4h", "12h"):
        interval = SUPPORTED[tf]
        df = _walk(max(400, 10 * DAY // interval), tf, seed=17, sigma=0.03 / (DAY // interval) ** 0.5)
        full = _apply(df, interval)
        for i in list(range(0, LIVE_MIN_CANDLES + 5)) + list(range(150, len(df), 37)):
            prefix = _apply(df.iloc[:i + 1].copy(), interval).iloc[-1]
            tail = _apply(df.iloc[max(0, i - 199):i + 1].copy(), interval).iloc[-1]
            for got in (prefix, tail):
                assert int(got["signal"]) == int(full["signal"].iloc[i]), (tf, i)
                assert got["cpr_reason"] == full["cpr_reason"].iloc[i], (tf, i)
                for col in ("cpr_s3", "cpr_r3", "cpr_s4", "cpr_r4"):
                    a, b = got[col], full[col].iloc[i]
                    assert (math.isnan(a) and math.isnan(b)) or a == b, (tf, i, col)
        assert (full["signal"] == 1).any() and (full["signal"] == -1).any(), tf


def _tz_variants(df):
    utc = df.copy()
    utc.index = utc.index.tz_localize("UTC")
    utc.attrs = dict(df.attrs)
    ny = utc.copy()
    ny.index = ny.index.tz_convert("America/New_York")
    ny.attrs = dict(df.attrs)
    native = df.copy()
    native.index = native.index + pd.Timedelta(milliseconds=int(df.attrs[FRAME_INTERVAL_ATTR]) - 1)
    native.attrs = dict(df.attrs)
    native.attrs[FRAME_TIMESTAMP_KIND_ATTR] = TIMESTAMP_KIND_NATIVE_CLOSE
    column = df.reset_index()
    column["timestamp"] = [int(t.value // 1_000_000) for t in df.index]
    column = column.drop(columns=["datetime"])
    column.attrs = dict(df.attrs)
    return {"aware_utc": utc, "america_new_york": ny, "native_close": native, "timestamp_column": column}


def test_naive_aware_new_york_and_native_close_inputs_give_the_same_sessions():
    for start in ("2026-03-06", "2026-03-27", "2026-10-30"):
        with _case(start):
            _naive_aware_new_york_and_native_close_inputs_give_the_same_sessions(start)


def _naive_aware_new_york_and_native_close_inputs_give_the_same_sessions(start):
    df = _walk(24 * 5, "1h", seed=23, start=start, sigma=0.012)
    base = _apply(df)
    cols = ["signal", "cpr_reason", "cpr_eval_ts_ms", "cpr_day_open_ms", "cpr_s3", "cpr_r3", "cpr_s4", "cpr_r4"]
    for label, variant in _tz_variants(df).items():
        got = _apply(variant)
        for col in cols:
            assert list(got[col].astype(str)) == list(base[col].astype(str)), (label, col)
    assert (base["cpr_valid"] == 1.0).sum() > 40


TIMING_ATTRS = [
    ({}, "undeclared_timing"),
    ({FRAME_INTERVAL_ATTR: H}, "undeclared_timing"),
    ({FRAME_TIMESTAMP_KIND_ATTR: TIMESTAMP_KIND_OPEN}, "undeclared_timing"),
    ({FRAME_INTERVAL_ATTR: 4 * H, FRAME_TIMESTAMP_KIND_ATTR: TIMESTAMP_KIND_OPEN}, "timing_contradiction"),
    ({FRAME_INTERVAL_ATTR: True, FRAME_TIMESTAMP_KIND_ATTR: TIMESTAMP_KIND_OPEN}, "timing_contradiction"),
    ({FRAME_INTERVAL_ATTR: H, FRAME_TIMESTAMP_KIND_ATTR: "close"}, "timing_contradiction"),
]


def _missing_or_contradictory_timing_cases():
    for attrs, reason in TIMING_ATTRS:
        with _case(attrs):
            _missing_or_contradictory_timing_holds_every_row(attrs, reason)


def _missing_or_contradictory_timing_holds_every_row(attrs, reason):
    df = _day_frame("1h", [100.0, 94.0, 95.0])
    df.attrs = dict(attrs)
    out = _apply(df)
    assert set(out["cpr_reason"]) == {reason} and int(out["signal"].abs().sum()) == 0
    no_ts = _day_frame("1h", [100.0, 94.0, 95.0]).reset_index(drop=True)
    out = _apply(no_ts)
    assert set(out["cpr_reason"]) == {"missing_timestamps"}


def test_input_defects_hold_the_decisions_they_feed_with_their_reason(subtests):
    with subtests.test(case="prior_day_defects_hold_every_decision_of_the_next_day"):
        _prior_day_defects_hold_every_decision_of_the_next_day()
    with subtests.test(case="both_crossing_candles_are_validated"):
        _both_crossing_candles_are_validated()
    with subtests.test(case="levels_never_carry_over_a_missing_day"):
        _levels_never_carry_over_a_missing_day()
    with subtests.test(case="missing_or_contradictory_timing_holds_every_row"):
        _missing_or_contradictory_timing_cases()


def _runtime(df, cutoff, boundary):
    out = df.copy()
    out.attrs = dict(df.attrs)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def _every_supported_interval_validates():
    for value in list(SUPPORTED.values()) + [3_600_000.0]:
        with _case(value):
            _reg()._registry.validate_params(NAME, {"expected_interval_ms": value})


UNSUPPORTED_INTERVALS = [60_000, 300_000, 21_600_000, 86_400_000, 0, -H, True, 3_600_000.5, float("nan"),
                         float("inf"), "3600000", None]


def _unsupported_or_malformed_intervals_are_refused():
    for value in UNSUPPORTED_INTERVALS:
        with _case(repr(value)):
            with pytest.raises(ValueError, match="expected_interval_ms"):
                _reg()._registry.validate_params(NAME, {"expected_interval_ms": value})
            with pytest.raises(ValueError):
                _apply(_day_frame("1h", [100.0]), value)


def test_interval_parameter_accepts_supported_cadences_and_refuses_the_rest(subtests):
    with subtests.test(case="every_supported_interval_validates"):
        _every_supported_interval_validates()
    with subtests.test(case="unsupported_or_malformed_intervals_are_refused"):
        _unsupported_or_malformed_intervals_are_refused()


def test_research_binding_refuses_a_cadence_or_timeframe_mismatch_before_loading_candles(monkeypatch):
    import data_fetcher

    def refuse(*_a, **_k):
        raise AssertionError("candles were loaded before the parameters were validated")

    monkeypatch.setattr(data_fetcher, "load_cached_data", refuse)
    reg = _reg()
    window = ("2026-01-01", "2026-02-01")
    with pytest.raises(ValueError, match="does not match timeframe"):
        eval_windows.run_leg(reg, NAME, {"expected_interval_ms": H}, "BTC/USDT", "4h", window)
    with pytest.raises(ValueError, match="expected_interval_ms >= 900000"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "5m", window)
    with pytest.raises(ValueError, match="not a supported interval"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "6h", window)
    with pytest.raises(ValueError, match="fixed-duration timeframe"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "1w", window)
    assert bind_fixed_interval_params({"expected_interval_ms": H}, None, "4h") == {"expected_interval_ms": 4 * H}


def test_check_preflight_refuses_cadence_mismatches_for_open_and_close_fallback_references():
    reg = _reg()
    refs = [StrategyReference("open", "breakout"), StrategyReference("close", NAME)]

    def run(params, timeframe):
        preflight_reference_params(refs, {}, {NAME: params}, reg.get_strategy, reg.validate_params,
                                   timeframe=timeframe)

    run({"expected_interval_ms": 4 * H}, "4h")
    run({}, "1h")
    for params, tf, needle in (({}, "4h", "registry default expected_interval_ms=3600000"),
                               ({"expected_interval_ms": H}, "4h", "does not match timeframe '4h'"),
                               ({}, "1w", "fixed-duration timeframe"),
                               ({"expected_interval_ms": 300_000}, "5m", "expected_interval_ms >= 900000"),
                               ({"expected_interval_ms": 21_600_000}, "6h", "not a supported interval")):
        with pytest.raises(ValueError) as exc:
            run(params, tf)
        assert str(exc.value).startswith(f"close strategy '{NAME}' has invalid parameters")
        assert needle in str(exc.value)
    preflight_reference_params(refs, {}, {NAME: {}}, reg.get_strategy, reg.validate_params)


def _cfg(**kw):
    base = dict(strategy_name=NAME, params={"expected_interval_ms": H}, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="1h", close_refs=json.loads(json.dumps(CLOSE)), direction="both",
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict")
    base.update(kw)
    return ParityConfig(**base)


def _study():
    spec = importlib.util.spec_from_file_location("_cpr_1669_study", STUDY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_study_local_reclaim_comparator_uses_the_same_contract_and_is_removed_after_a_failure():
    study = _study()
    reg = _reg()
    df = _day_frame("1h", [100.0, 89.5, 90.5])
    out = study.prior_day_reclaim(df)
    cand = _apply(df)
    assert int(out["signal"].iloc[26]) == 1 and int(cand["signal"].iloc[26]) == 0
    short = study.prior_day_reclaim(_day_frame("1h", [100.0, 110.5, 109.5]))
    assert int(short["signal"].iloc[26]) == -1
    breach = _set(df, 25, "low", S4)
    assert int(study.prior_day_reclaim(breach)["signal"].abs().sum()) == 0
    with pytest.raises(RuntimeError):
        with study.installed_comparator(reg):
            assert study.LOCAL_COMPARATOR in reg.STRATEGY_REGISTRY
            got = reg.apply_strategy(study.LOCAL_COMPARATOR, df, {"expected_interval_ms": H})
            assert int(got["signal"].iloc[26]) == 1
            raise RuntimeError("comparison failed")
    assert study.LOCAL_COMPARATOR not in reg.STRATEGY_REGISTRY
    assert study.LOCAL_COMPARATOR not in reg._registry.STRATEGIES
