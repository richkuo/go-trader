import os
import sys

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_ROOT, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

_FUTURES = load_module("_invfvg_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module("_invfvg_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_REF = load_module("_invfvg_study_reference_test",
                   os.path.join(_ROOT, "backtest", "candidates", "inversion_fvg_2012", "reference.py"))

NAME = "inversion_fvg"
SMALL = {"min_fvg_atr": 0.25, "max_gap_age": 10, "retest_window": 10, "entry_level": 0.5}
STEP_MS = 14_400_000
T0_MS = 1_767_225_600_000
REGISTRIES = {"futures": _FUTURES, "spot": _SPOT}
VALID_REASONS = {"entry_long", "entry_short", "no_armed_zone", "awaiting_retest", "zone_armed_this_bar",
                 "zone_invalidated", "short_suppressed_spot", "both_sides_qualified"}
PRE = 40


def module():
    return sys.modules["inversion_fvg"]


def flat(px, n=1):
    return [(px, px + 0.5, px - 0.5, px)] * n


def mirror(rows):
    return [(200.0 - o, 200.0 - lo, 200.0 - h, 200.0 - c) for o, h, lo, c in rows]


def frame(rows, timestamps=None):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 10.0
    if timestamps is None:
        timestamps = [T0_MS + i * STEP_MS for i in range(len(rows))]
    df.index = pd.to_datetime(pd.Series(timestamps, dtype="float64"), unit="ms")
    return df, [float(x) for x in timestamps]


def run(platform, df, params=SMALL):
    return REGISTRIES[platform].apply_strategy(NAME, df, dict(params))


def assert_matches_reference(rows, params=SMALL, timestamps=None, modes=("retest",)):
    df, ts = frame(rows, timestamps)
    for mode in modes:
        for platform, allow_short in (("futures", True), ("spot", False)):
            if mode == "retest":
                out = run(platform, df, params)
            else:
                out = module()._evaluate(df, params["min_fvg_atr"], params["max_gap_age"], params["retest_window"],
                                         params["entry_level"], allow_short=allow_short, mode=mode)
            want_sig, want_reason = _REF.reference(rows, ts, params, allow_short, mode)
            assert out["signal"].tolist() == want_sig, (mode, platform)
            assert out["invfvg_reason"].tolist() == want_reason, (mode, platform)
            for i, reason in enumerate(want_reason):
                assert bool(out["invfvg_valid"].iloc[i]) is (reason in VALID_REASONS), (mode, platform, i)


GAP_BAR = PRE + 1
K1 = (100.0, 100.0, 99.0, 99.0)
GAP = (99.0, 99.25, 98.25, 98.5)
WAIT = (98.5, 99.0, 98.0, 98.5)


def bearish_gap(pre=PRE, gap=GAP):
    return flat(100.0, pre) + [K1, gap]


def invert(close=100.0):
    return (98.5, max(100.5, close), 98.0, close)


def long_setup(waits=2, inversion=100.0, after=(), retest=(100.0, 100.25, 99.3, 99.6)):
    rows = bearish_gap() + [WAIT] * waits + [invert(inversion)] + list(after)
    if retest is not None:
        rows.append(retest)
    return rows


def last(platform, rows, params=SMALL, timestamps=None):
    return run(platform, frame(rows, timestamps)[0], params).iloc[-1]


def test_a_bearish_gap_closed_above_and_retested_enters_long_and_its_mirror_enters_short_on_futures_only():
    rows = long_setup()
    long_out = last("futures", rows)
    assert (int(long_out["signal"]), long_out["invfvg_reason"]) == (1, "entry_long")
    assert long_out["invfvg_long_zone_bottom"] == 99.25 and long_out["invfvg_long_zone_top"] == 99.5
    assert long_out["invfvg_long_level"] == 99.375 and long_out["invfvg_long_gap_atr"] == 1.0
    assert long_out["invfvg_long_gap_ts_ms"] == float(T0_MS + GAP_BAR * STEP_MS)
    assert long_out["invfvg_long_armed_ts_ms"] == float(T0_MS + (GAP_BAR + 3) * STEP_MS)
    assert long_out["invfvg_long_bars_since_armed"] == 1.0
    assert int(last("spot", rows)["signal"]) == 1
    short_rows = mirror(rows)
    fut = last("futures", short_rows)
    spot = last("spot", short_rows)
    assert (int(fut["signal"]), fut["invfvg_reason"]) == (-1, "entry_short")
    assert fut["invfvg_short_zone_bottom"] == 100.5 and fut["invfvg_short_zone_top"] == 100.75
    assert fut["invfvg_short_level"] == 100.625
    assert (int(spot["signal"]), spot["invfvg_reason"]) == (0, "short_suppressed_spot")
    assert spot["invfvg_short_qualified"] == 1.0 and spot["invfvg_short_entries"] == 0.0
    assert fut["invfvg_short_entries"] == 1.0
    assert_matches_reference(rows)
    assert_matches_reference(short_rows)


def test_gap_and_inversion_comparisons_are_strict():
    touching = (99.0, 99.5, 98.25, 98.5)
    rows = flat(100.0, PRE) + [K1, touching] + [WAIT] * 2 + [invert()] + [(100.0, 100.25, 99.3, 99.6)]
    out = run("futures", frame(rows)[0])
    assert (out["signal"] == 0).all() and out["invfvg_reason"].iloc[-1] == "no_armed_zone"
    assert_matches_reference(rows)
    at_top = long_setup(inversion=99.5, retest=None)
    out = last("futures", at_top)
    assert out["invfvg_reason"] == "no_armed_zone"
    above = long_setup(inversion=99.75, retest=None)
    out = last("futures", above)
    assert out["invfvg_reason"] == "zone_armed_this_bar" and out["invfvg_long_zone_top"] == 99.5
    assert_matches_reference(at_top)
    assert_matches_reference(above)


def test_the_width_filter_admits_a_gap_exactly_at_the_atr_threshold():
    rows = long_setup()
    at = last("futures", rows, dict(SMALL, min_fvg_atr=0.25))
    assert at["invfvg_long_gap_atr"] == 1.0 and int(at["signal"]) == 1
    over = last("futures", rows, dict(SMALL, min_fvg_atr=0.2500001))
    assert int(over["signal"]) == 0 and over["invfvg_reason"] == "no_armed_zone"
    for params in (dict(SMALL, min_fvg_atr=0.0), dict(SMALL, min_fvg_atr=0.2500001)):
        assert_matches_reference(rows, params)


def test_entry_levels_zero_and_one_sit_on_the_zone_bounds():
    bottom_only = (100.0, 100.25, 99.25, 99.3)
    top_only = (100.0, 100.25, 99.5, 99.6)
    for level, retest, want in ((0.0, bottom_only, 1), (0.0, top_only, 0), (1.0, top_only, 1),
                                (1.0, (100.0, 100.25, 99.51, 99.6), 0)):
        rows = long_setup(retest=retest)
        out = last("futures", rows, dict(SMALL, entry_level=level))
        assert int(out["signal"]) == want, (level, retest)
        assert_matches_reference(rows, dict(SMALL, entry_level=level))


def test_inversion_and_retest_age_limits_are_inclusive():
    params = dict(SMALL, max_gap_age=6, retest_window=4)
    last_inversion = long_setup(waits=5, retest=None)
    assert last("futures", last_inversion, params)["invfvg_reason"] == "zone_armed_this_bar"
    expired_gap = long_setup(waits=6, retest=None)
    assert last("futures", expired_gap, params)["invfvg_reason"] == "no_armed_zone"
    above = (100.0, 100.5, 99.6, 100.0)
    last_retest = long_setup(after=[above] * 3)
    out = last("futures", last_retest, params)
    assert (int(out["signal"]), out["invfvg_long_bars_since_armed"]) == (1, 4.0)
    expired_zone = long_setup(after=[above] * 4)
    out = last("futures", expired_zone, params)
    assert int(out["signal"]) == 0 and out["invfvg_reason"] == "no_armed_zone"
    for rows in (last_inversion, expired_gap, last_retest, expired_zone):
        assert_matches_reference(rows, params)


def test_the_inversion_bar_cannot_retest_and_candles_wholly_above_the_level_never_qualify():
    rows = long_setup(retest=None)
    df, _ = frame(rows)
    out = run("futures", df)
    assert out["invfvg_reason"].iloc[-1] == "zone_armed_this_bar" and int(out["signal"].iloc[-1]) == 0
    above = (100.0, 100.5, 99.4, 100.0)
    rows = long_setup(after=[above] * 3)
    out = run("futures", frame(rows)[0])
    assert out["invfvg_reason"].tolist()[-4:-1] == ["awaiting_retest"] * 3
    assert out["signal"].tolist()[-4:] == [0, 0, 0, 1]
    assert_matches_reference(rows)


def test_a_close_below_the_zone_clears_it_and_a_held_close_is_required():
    breaking = (99.6, 99.7, 99.0, 99.2)
    rows = long_setup(retest=breaking)
    out = last("futures", rows)
    assert int(out["signal"]) == 0 and out["invfvg_reason"] == "zone_invalidated"
    rows = long_setup(after=[breaking], retest=(99.2, 99.5, 99.2, 99.4))
    out = last("futures", rows)
    assert int(out["signal"]) == 0 and out["invfvg_reason"] == "no_armed_zone"
    assert_matches_reference(rows)


def test_each_zone_fires_once_and_repeat_touches_after_consumption_hold():
    touch = (100.0, 100.25, 99.3, 99.6)
    rows = long_setup(retest=touch) + [touch] * 4
    out = run("futures", frame(rows)[0])
    assert out["signal"].tolist()[-5:] == [1, 0, 0, 0, 0]
    assert out["invfvg_reason"].tolist()[-4:] == ["no_armed_zone"] * 4
    assert_matches_reference(rows)


REPLACEMENT_PATH = [(100.0, 101.5, 99.8, 101.5), (101.5, 103.0, 101.4, 102.8), (102.8, 103.0, 102.0, 102.2),
                    (102.2, 102.3, 101.0, 101.2), (101.2, 101.5, 100.5, 100.8)]
REPLACE_AND_TOUCH = (100.8, 103.5, 99.3, 103.0)


def test_a_newer_inversion_replaces_the_armed_zone_even_on_its_retest_bar():
    without = long_setup(after=REPLACEMENT_PATH[:2], retest=REPLACE_AND_TOUCH)
    out = last("futures", without)
    assert (int(out["signal"]), out["invfvg_long_zone_bottom"]) == (1, 99.25)
    rows = long_setup(after=REPLACEMENT_PATH, retest=REPLACE_AND_TOUCH)
    out = last("futures", rows)
    assert out["invfvg_reason"] == "zone_armed_this_bar" and int(out["signal"]) == 0
    assert out["invfvg_long_zone_bottom"] == 101.5 and out["invfvg_long_zone_top"] == 102.0
    retest_new = rows + [(103.0, 103.2, 101.7, 102.5), (102.5, 103.0, 99.3, 102.5)]
    out = run("futures", frame(retest_new)[0])
    assert out["signal"].tolist()[-2:] == [1, 0]
    assert out["invfvg_long_zone_bottom"].iloc[-2] == 101.5
    for case in (without, rows, retest_new):
        assert_matches_reference(case)


def test_same_bar_inversions_keep_the_greatest_gap_bar():
    rows = (flat(100.0, PRE) + [K1, GAP, (98.5, 98.75, 98.0, 98.25), (98.25, 98.25, 97.0, 97.5),
                                  (97.5, 97.75, 96.5, 97.0)] + [(97.0, 101.0, 96.9, 100.0)])
    params = dict(SMALL, min_fvg_atr=0.0)
    out = run("futures", frame(rows)[0], params)
    assert out["invfvg_reason"].iloc[-1] == "zone_armed_this_bar"
    assert out["invfvg_long_gap_ts_ms"].iloc[-1] == float(T0_MS + (len(rows) - 2) * STEP_MS)
    assert (out["invfvg_long_zone_bottom"].iloc[-1], out["invfvg_long_zone_top"].iloc[-1]) == (97.75, 98.0)
    assert_matches_reference(rows, params)


def test_opposite_qualifications_on_one_bar_hold_and_consume_both_zones_before_spot_suppression():
    walk_rows = _walk(1500, 9)
    out_f = run("futures", frame(walk_rows)[0], dict(SMALL, min_fvg_atr=0.0))
    out_s = run("spot", frame(walk_rows)[0], dict(SMALL, min_fvg_atr=0.0))
    mask = out_f["invfvg_reason"] == "both_sides_qualified"
    assert mask.any()
    assert (out_f.loc[mask, "invfvg_long_qualified"] == 1.0).all()
    assert (out_f.loc[mask, "invfvg_short_qualified"] == 1.0).all()
    assert (out_f.loc[mask, "signal"] == 0).all() and (out_s.loc[mask, "signal"] == 0).all()
    assert (out_s.loc[mask, "invfvg_reason"] == "both_sides_qualified").all()
    assert_matches_reference(walk_rows, dict(SMALL, min_fvg_atr=0.0))


def _walk(n, seed, sigma=0.02):
    rng = np.random.default_rng(seed)
    close = 100.0 * np.exp(np.cumsum(rng.normal(0.0, sigma, n)))
    out = []
    prev = 100.0
    for i in range(n):
        c = float(close[i])
        o = float(prev)
        wick = rng.uniform(0.0005, 0.01, 2)
        out.append((o, max(o, c) * (1 + wick[0]), min(o, c) * (1 - wick[1]), c))
        prev = c
    return out


GRID = [
    {"min_fvg_atr": 0.25, "max_gap_age": 48, "retest_window": 24, "entry_level": 0.5},
    {"min_fvg_atr": 0.0, "max_gap_age": 24, "retest_window": 12, "entry_level": 0.25},
    {"min_fvg_atr": 1.0, "max_gap_age": 96, "retest_window": 48, "entry_level": 0.75},
    {"min_fvg_atr": 0.0, "max_gap_age": 96, "retest_window": 89, "entry_level": 0.5},
    {"min_fvg_atr": 0.0, "max_gap_age": 3, "retest_window": 2, "entry_level": 1.0},
]


def test_signals_and_reasons_match_the_independent_reference_on_random_walks(subtests):
    for seed in (1, 2, 3):
        rows = _walk(700, seed)
        for params in GRID:
            with subtests.test(seed=seed, params=params):
                assert_matches_reference(rows, params,
                                         modes=("retest", "noninverted_retest", "inversion_close"))
    entries = run("futures", frame(_walk(700, 1))[0], GRID[0])["signal"]
    assert (entries == 1).sum() >= 5 and (entries == -1).sum() >= 5


def test_spot_never_emits_a_short_entry_and_equals_the_futures_longs():
    for seed in (4, 5):
        df, _ = frame(_walk(800, seed))
        fut = run("futures", df, GRID[1])
        spot = run("spot", df, GRID[1])
        assert (spot["signal"] >= 0).all() and (fut["signal"] == -1).any()
        assert (spot["signal"].to_numpy() == np.maximum(fut["signal"].to_numpy(), 0)).all()
        assert (spot.loc[fut["signal"] == -1, "invfvg_reason"] == "short_suppressed_spot").all()


def test_spot_short_cannot_be_enabled_by_a_parameter():
    df, _ = frame(mirror(long_setup()))
    for extra in ({"allow_short": True}, {"short_entries": True}, {"mode": "inversion_close"}):
        with pytest.raises((TypeError, ValueError)):
            run("spot", df, dict(SMALL, **extra))


INVALID_PARAMS = [
    {"min_fvg_atr": -0.1}, {"min_fvg_atr": 10.5}, {"min_fvg_atr": float("nan")}, {"min_fvg_atr": True},
    {"min_fvg_atr": "0.25"}, {"max_gap_age": 0}, {"max_gap_age": 201}, {"max_gap_age": 12.5},
    {"max_gap_age": True}, {"retest_window": 0}, {"retest_window": "24"}, {"entry_level": -0.01},
    {"entry_level": 1.01}, {"entry_level": False}, {"max_gap_age": 150, "retest_window": 36},
    {"max_gap_age": 186, "retest_window": 1},
]


def test_parameter_bounds_and_the_combined_support_bound_are_configuration_errors(subtests):
    df, _ = frame(long_setup())
    for bad in INVALID_PARAMS:
        params = dict(SMALL, **bad)
        with subtests.test(bad=bad):
            with pytest.raises(ValueError):
                _FUTURES._registry.validate_params(NAME, params)
            for platform in ("futures", "spot"):
                with pytest.raises(ValueError):
                    run(platform, df, params)
            with pytest.raises(ValueError):
                module().inversion_fvg_futures(df, **params)
    with pytest.raises(ValueError):
        _FUTURES._registry.validate_params(NAME, dict(SMALL, lookback=5))
    edge = dict(SMALL, max_gap_age=150, retest_window=35)
    assert module().required_history_bars(150, 35) == 200
    _FUTURES._registry.validate_params(NAME, edge)
    assert module().required_history_bars(48, 24) == 87


DEFECTS = {
    "nan_close": (lambda r: (r[0], r[1], r[2], float("nan")), "nonfinite_input"),
    "infinite_high": (lambda r: (r[0], float("inf"), r[2], r[3]), "nonfinite_input"),
    "zero_price": (lambda r: (0.0, r[1], 0.0, r[3]), "nonpositive_price"),
    "high_below_low": (lambda r: (r[0], r[2], r[1], r[3]), "inconsistent_ohlc"),
    "close_above_high": (lambda r: (r[0], r[1], r[2], r[1] + 1.0), "inconsistent_ohlc"),
}


def test_defects_inside_the_span_hold_with_their_reason_and_the_strategy_recovers(subtests):
    support = module().required_history_bars(SMALL["max_gap_age"], SMALL["retest_window"])
    for case, (mutate, reason) in sorted(DEFECTS.items()):
        with subtests.test(case=case):
            final = len(long_setup()) - 1
            for at in (final - support + 1, GAP_BAR - 14, GAP_BAR, GAP_BAR + 2, final):
                rows = long_setup()
                rows[at] = mutate(rows[at])
                out = run("futures", frame(rows)[0])
                assert int(out["signal"].iloc[-1]) == 0 and out["invfvg_reason"].iloc[-1] == reason, at
                assert not bool(out["invfvg_valid"].iloc[-1])
                assert np.isnan(out["invfvg_long_zone_bottom"].iloc[-1])
                assert_matches_reference(rows)
            rows = long_setup()
            rows[final - support] = mutate(rows[final - support])
            out = run("futures", frame(rows)[0])
            assert int(out["signal"].iloc[-1]) == 1
            defect = list(long_setup())
            defect[5] = mutate(defect[5])
            recovered = defect + flat(100.0, support) + long_setup()[PRE - 16:]
            out = run("futures", frame(recovered)[0])
            assert int(out["signal"].iloc[-1]) == 1 and out["invfvg_reason"].iloc[-1] == "entry_long"
            held = out["invfvg_reason"].iloc[5:5 + support].tolist()
            assert held == [reason] * support
            assert_matches_reference(recovered)


def test_timestamp_defects_hold_inside_the_span_and_recover(subtests):
    rows = long_setup()
    base = [T0_MS + i * STEP_MS for i in range(len(rows))]
    cases = {
        "duplicate": (lambda ts: ts[:GAP_BAR] + [ts[GAP_BAR - 1]] + ts[GAP_BAR + 1:], "timestamp_order"),
        "reversed": (lambda ts: ts[:GAP_BAR] + [ts[GAP_BAR - 1] - 1] + ts[GAP_BAR + 1:], "timestamp_order"),
        "missing": (lambda ts: ts[:GAP_BAR] + [float("nan")] + ts[GAP_BAR + 1:], "missing_timestamp"),
    }
    for case, (mutate, reason) in sorted(cases.items()):
        with subtests.test(case=case):
            ts = mutate(list(base))
            out = run("futures", frame(rows, ts)[0])
            assert out["invfvg_reason"].iloc[-1] == reason and int(out["signal"].iloc[-1]) == 0
            assert_matches_reference(rows, timestamps=ts)
    ts = list(base)
    ts[2] = ts[1]
    out = run("futures", frame(rows, ts)[0])
    assert out["invfvg_reason"].iloc[-1] == "entry_long"
    assert_matches_reference(rows, timestamps=ts)


def test_insufficient_history_holds_until_the_full_support_span_exists():
    support = module().required_history_bars(SMALL["max_gap_age"], SMALL["retest_window"])
    rows = long_setup()
    trimmed = rows[len(rows) - support + 1:]
    out = run("futures", frame(trimmed)[0])
    assert (out["invfvg_reason"] == "insufficient_history").all() and (out["signal"] == 0).all()
    assert out["invfvg_required_history"].iloc[-1] == float(support)
    exact = rows[len(rows) - support:]
    out = run("futures", frame(exact)[0])
    assert out["invfvg_reason"].iloc[-1] == "entry_long"
    assert (out["invfvg_reason"].iloc[:support - 1] == "insufficient_history").all()


def test_derived_overflow_holds_with_a_reason():
    huge = [(1e308, 1.5e308, 1e307, 1e308)] * 40 + [(1e308, 1.5e308, 1e307, 1.4e308)]
    out = run("futures", frame(huge)[0], dict(SMALL, max_gap_age=5, retest_window=5))
    assert out["invfvg_reason"].iloc[-1] == "nonfinite_derived" and int(out["signal"].iloc[-1]) == 0
    assert_matches_reference(huge, dict(SMALL, max_gap_age=5, retest_window=5))


def test_missing_price_columns_hold_every_row_and_volume_is_not_read():
    df, _ = frame(long_setup())
    out = run("futures", df.drop(columns=["low"]))
    assert (out["invfvg_reason"] == "missing_columns").all() and (out["signal"] == 0).all()
    no_volume = run("futures", df.drop(columns=["volume"]))
    assert int(no_volume["signal"].iloc[-1]) == 1
