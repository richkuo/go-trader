import math
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

_FUTURES = load_module("_vsb_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module("_vsb_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))

NAME = "volume_spike_breakout"
SMALL = {"lookback": 3, "vol_period": 4, "vol_multiplier": 2.0, "min_body_frac": 0.0}
STEP_MS = 14_400_000
T0_MS = 1_767_225_600_000
REGISTRIES = {"futures": _FUTURES, "spot": _SPOT}
VALID_REASONS = {"entry_long", "entry_short", "no_breakout", "volume_below_threshold", "body_below_threshold",
                 "breakout_continuation", "short_suppressed_spot", "both_sides_qualified"}


def reference(rows, timestamps, params, allow_short):
    lb, vp, mult, body = (params["lookback"], params["vol_period"], params["vol_multiplier"],
                          params["min_body_frac"])
    span = max(lb, vp)
    n = len(rows)

    def row_reason(j):
        o, h, lo, c, v = rows[j]
        if not all(math.isfinite(x) for x in (o, h, lo, c, v)):
            return "nonfinite_input"
        if min(o, h, lo, c) <= 0:
            return "nonpositive_price"
        if v <= 0:
            return "invalid_volume"
        if h < lo or not lo <= o <= h or not lo <= c <= h:
            return "inconsistent_ohlc"
        if timestamps is not None and not math.isfinite(timestamps[j]):
            return "missing_timestamp"
        return ""

    def step_bad(j):
        if timestamps is None or j == 0:
            return False
        a, b = timestamps[j - 1], timestamps[j]
        return math.isfinite(a) and math.isfinite(b) and not b > a

    def condition(t):
        if t - span < 0:
            return None
        if any(row_reason(j) for j in range(t - span, t + 1)):
            return None
        if any(step_bad(j) for j in range(t - span + 1, t + 1)):
            return None
        high = max(rows[j][1] for j in range(t - lb, t))
        low = min(rows[j][2] for j in range(t - lb, t))
        total = 0.0
        for j in range(t - vp, t):
            total += rows[j][4]
        base = total / vp
        if not math.isfinite(base) or base <= 0:
            return None
        rel = rows[t][4] / base
        if not math.isfinite(rel):
            return None
        o, h, lo, c, _ = rows[t]
        frac = abs(c - o) / (h - lo) if h > lo else None
        body_ok = body == 0 or (frac is not None and frac >= body)
        up, down = c > high, c < low
        vol_ok = rel > mult
        return {"long": up and vol_ok and body_ok, "short": down and vol_ok and body_ok, "up": up, "down": down,
                "vol_ok": vol_ok, "body_ok": body_ok}

    signals, reasons = [], []
    for t in range(n):
        cur = condition(t)
        prev = condition(t - 1) if t >= 1 else None
        if cur is None or prev is None:
            signals.append(0)
            reasons.append(None)
            continue
        long_edge = cur["long"] and not prev["long"]
        short_edge = cur["short"] and not prev["short"]
        if long_edge and short_edge:
            signals.append(0)
            reasons.append("both_sides_qualified")
        elif long_edge:
            signals.append(1)
            reasons.append("entry_long")
        elif short_edge:
            signals.append(-1 if allow_short else 0)
            reasons.append("entry_short" if allow_short else "short_suppressed_spot")
        elif (cur["long"] and prev["long"]) or (cur["short"] and prev["short"]):
            signals.append(0)
            reasons.append("breakout_continuation")
        elif (cur["up"] or cur["down"]) and not cur["vol_ok"]:
            signals.append(0)
            reasons.append("volume_below_threshold")
        elif (cur["up"] or cur["down"]) and not cur["body_ok"]:
            signals.append(0)
            reasons.append("body_below_threshold")
        else:
            signals.append(0)
            reasons.append("no_breakout")
    return signals, reasons


def frame(rows, timestamps=None):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    if timestamps is None:
        timestamps = [T0_MS + i * STEP_MS for i in range(len(rows))]
    df.index = pd.to_datetime(pd.Series(timestamps, dtype="float64"), unit="ms")
    return df, [float(x) for x in timestamps]


def run(platform, df, params=SMALL):
    return REGISTRIES[platform].apply_strategy(NAME, df, params)


def bar(px, vol, span=1.0, body=None):
    o = px if body is None else px - body
    return (o, max(o, px) + span / 2, min(o, px) - span / 2, px, vol)


def flat(n, px=100.0, vol=10.0):
    return [bar(px, vol) for _ in range(n)]


def walk(n, seed, spike_every=9):
    rng = np.random.default_rng(seed)
    close = 100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.012, n)))
    out = []
    prev = 100.0
    for i in range(n):
        c = float(close[i])
        o = float(prev)
        wick = float(rng.uniform(0.0005, 0.01))
        vol = float(rng.uniform(5.0, 15.0)) * (float(rng.uniform(2.0, 6.0)) if i % spike_every == 0 else 1.0)
        out.append((o, max(o, c) * (1 + wick), min(o, c) * (1 - wick), c, vol))
        prev = c
    return out


def assert_matches_reference(rows, params, timestamps=None, check_reasons=True):
    df, ts = frame(rows, timestamps)
    for platform, allow_short in (("futures", True), ("spot", False)):
        out = run(platform, df, params)
        want_sig, want_reason = reference(rows, ts, params, allow_short)
        assert out["signal"].tolist() == want_sig, platform
        if check_reasons:
            for i, want in enumerate(want_reason):
                got = out["vsb_reason"].iloc[i]
                if want is None:
                    assert got not in VALID_REASONS and not bool(out["vsb_valid"].iloc[i]), (platform, i, got)
                else:
                    assert got == want, (platform, i, got, want)
                    assert bool(out["vsb_valid"].iloc[i]), (platform, i)


SHAPES = {
    "flat": lambda: flat(40),
    "rising": lambda: [bar(100.0 + i, 10.0 if i % 7 else 40.0) for i in range(60)],
    "falling": lambda: [bar(200.0 - i, 10.0 if i % 7 else 40.0) for i in range(60)],
    "spike_without_breakout": lambda: flat(20) + [bar(100.0, 200.0)] + flat(10),
    "breakout_with_spike": lambda: flat(20) + [bar(110.0, 50.0, body=9.0)] + [bar(111.0, 50.0)] + flat(5, 111.0),
    "zero_volume": lambda: flat(10) + [bar(100.0, 0.0)] + flat(10) + [bar(115.0, 60.0)],
    "nan_close": lambda: flat(10) + [(100.0, 100.5, 99.5, float("nan"), 10.0)] + flat(10) + [bar(85.0, 60.0)],
}


def test_signals_and_reasons_match_an_independent_reference_on_shaped_fixtures(subtests):
    for shape, build in sorted(SHAPES.items()):
        for params in (SMALL, dict(SMALL, min_body_frac=0.5), dict(SMALL, lookback=6, vol_period=2)):
            with subtests.test(shape=shape, params=params):
                assert_matches_reference(build(), params)


def test_signals_and_reasons_match_an_independent_reference_on_random_walks(subtests):
    grid = [
        {"lookback": 20, "vol_period": 20, "vol_multiplier": 2.0, "min_body_frac": 0.0},
        {"lookback": 10, "vol_period": 40, "vol_multiplier": 1.5, "min_body_frac": 0.25},
        {"lookback": 40, "vol_period": 10, "vol_multiplier": 1.0, "min_body_frac": 0.5},
        {"lookback": 100, "vol_period": 100, "vol_multiplier": 3.0, "min_body_frac": 0.0},
    ]
    for seed in (1, 2, 3):
        rows = walk(600, seed)
        for params in grid:
            with subtests.test(seed=seed, params=params):
                assert_matches_reference(rows, params)
    entries = sum(int(x != 0) for x in run("futures", frame(walk(600, 1))[0], grid[0])["signal"])
    assert entries >= 4


def test_strict_threshold_and_range_equality_hold():
    rows = flat(8) + [bar(100.5, 20.0)]
    df, _ = frame(rows)
    last = run("futures", df).iloc[-1]
    assert last["vsb_range_high"] == 100.5 and last["close"] == 100.5
    assert int(last["signal"]) == 0 and last["vsb_reason"] == "no_breakout"
    rows = flat(8) + [bar(110.0, 20.0)]
    last = run("futures", frame(rows)[0]).iloc[-1]
    assert last["vsb_rel_vol"] == 2.0
    assert int(last["signal"]) == 0 and last["vsb_reason"] == "volume_below_threshold"
    rows = flat(8) + [bar(110.0, 20.5)]
    assert int(run("futures", frame(rows)[0]).iloc[-1]["signal"]) == 1


def test_current_bar_is_excluded_from_its_own_range_and_baseline():
    rows = flat(8) + [(100.0, 130.0, 99.5, 125.0, 1000.0)]
    last = run("futures", frame(rows)[0]).iloc[-1]
    assert last["vsb_range_high"] == 100.5
    assert last["vsb_base_volume"] == 10.0
    assert last["vsb_rel_vol"] == 100.0
    assert int(last["signal"]) == 1 and last["vsb_reason"] == "entry_long"


def test_spot_never_emits_a_short_entry_and_futures_mirrors_it():
    rows = flat(8) + [bar(90.0, 50.0)]
    df, _ = frame(rows)
    fut = run("futures", df).iloc[-1]
    spot = run("spot", df).iloc[-1]
    assert (int(fut["signal"]), fut["vsb_reason"]) == (-1, "entry_short")
    assert (int(spot["signal"]), spot["vsb_reason"]) == (0, "short_suppressed_spot")
    assert spot["vsb_short_qualified"] == 1.0 and spot["vsb_short_entries"] == 0.0
    assert fut["vsb_short_entries"] == 1.0
    for seed in (4, 5):
        out = run("spot", frame(walk(500, seed))[0], {"lookback": 10, "vol_period": 10, "vol_multiplier": 1.0,
                                                       "min_body_frac": 0.0})
        assert (out["signal"] >= 0).all()
        assert (out["vsb_reason"] == "short_suppressed_spot").any()


def test_spot_short_cannot_be_enabled_by_a_parameter():
    df, _ = frame(flat(8) + [bar(90.0, 50.0)])
    for extra in ({"allow_short": True}, {"short_entries": True}):
        with pytest.raises((TypeError, ValueError)):
            run("spot", df, dict(SMALL, **extra))


def test_a_new_volume_qualification_after_a_false_bar_is_a_new_event():
    rows = flat(8) + [bar(110.0, 50.0), bar(111.0, 12.0), bar(112.0, 80.0)]
    out = run("futures", frame(rows)[0])
    assert out["signal"].tolist()[-3:] == [1, 0, 1]
    assert out["vsb_reason"].tolist()[-3:] == ["entry_long", "volume_below_threshold", "entry_long"]


def test_body_filter_rejects_wicks_and_zero_range_and_recovers_by_the_event_rule():
    params = dict(SMALL, min_body_frac=0.5)
    wick = (100.0, 112.0, 99.5, 101.0, 50.0)
    out = run("futures", frame(flat(8) + [wick])[0], params).iloc[-1]
    assert int(out["signal"]) == 0 and out["vsb_reason"] == "body_below_threshold"
    zero_range = (101.0, 101.0, 101.0, 101.0, 50.0)
    out = run("futures", frame(flat(8) + [zero_range])[0], params).iloc[-1]
    assert np.isnan(out["vsb_body_frac"]) and out["vsb_reason"] == "body_below_threshold"
    assert int(run("futures", frame(flat(8) + [zero_range])[0], SMALL).iloc[-1]["signal"]) == 1
    rows = flat(8) + [wick, bar(113.0, 60.0, body=6.0)]
    out = run("futures", frame(rows)[0], params)
    assert out["signal"].tolist()[-2:] == [0, 1]
    assert out["vsb_reason"].tolist()[-2:] == ["body_below_threshold", "entry_long"]


DEFECTS = {
    "nan_volume": (lambda r: (r[0], r[1], r[2], r[3], float("nan")), "nonfinite_input"),
    "zero_volume": (lambda r: (r[0], r[1], r[2], r[3], 0.0), "invalid_volume"),
    "negative_volume": (lambda r: (r[0], r[1], r[2], r[3], -5.0), "invalid_volume"),
    "zero_price": (lambda r: (0.0, r[1], 0.0, r[3], r[4]), "nonpositive_price"),
    "high_below_low": (lambda r: (r[0], r[2], r[1], r[3], r[4]), "inconsistent_ohlc"),
    "close_above_high": (lambda r: (r[0], r[1], r[2], r[1] + 1.0, r[4]), "inconsistent_ohlc"),
    "open_below_low": (lambda r: (r[2] - 1.0, r[1], r[2], r[3], r[4]), "inconsistent_ohlc"),
    "infinite_high": (lambda r: (r[0], float("inf"), r[2], r[3], r[4]), "nonfinite_input"),
}


def test_defective_bars_hold_with_a_reason_and_never_invent_volume(subtests):
    for case, (mutate, reason) in sorted(DEFECTS.items()):
        with subtests.test(case=case):
            rows = flat(9) + [bar(110.0, 50.0)]
            rows[-1] = mutate(rows[-1])
            df, ts = frame(rows)
            out = run("futures", df)
            last = out.iloc[-1]
            assert int(last["signal"]) == 0 and not bool(last["vsb_valid"])
            assert last["vsb_reason"] == reason
            assert np.isnan(last["vsb_rel_vol"]) and np.isnan(last["vsb_base_volume"])
            assert out["volume"].iloc[-1] == df["volume"].iloc[-1] or np.isnan(df["volume"].iloc[-1])
            assert_matches_reference(rows, SMALL)


def test_a_malformed_preceding_bar_holds_and_cannot_manufacture_an_edge(subtests):
    for case, (mutate, reason) in sorted(DEFECTS.items()):
        with subtests.test(case=case):
            rows = flat(9) + [bar(110.0, 50.0), bar(111.0, 60.0)]
            rows[-2] = mutate(rows[-2])
            out = run("futures", frame(rows)[0])
            assert int(out["signal"].iloc[-1]) == 0 and out["vsb_reason"].iloc[-1] == reason
            assert not bool(out["vsb_valid"].iloc[-1])
            assert_matches_reference(rows, SMALL)


def test_recovery_after_a_defect_follows_the_event_rule():
    defect = (100.0, 100.5, 99.5, 100.0, float("nan"))
    rows = flat(5) + [defect] + flat(4) + [bar(110.0, 50.0), bar(111.0, 60.0), bar(112.0, 70.0)]
    out = run("futures", frame(rows)[0])
    reasons = out["vsb_reason"].tolist()
    assert reasons[5:10] == ["nonfinite_input"] * 5
    assert reasons[10] == "nonfinite_input"
    assert bool(out["vsb_valid"].iloc[11]) and out["signal"].iloc[11] == 0
    assert reasons[11] == "breakout_continuation"
    assert out["signal"].iloc[12] == 0
    assert_matches_reference(rows, SMALL)
    clear = flat(5) + [defect] + flat(6) + [bar(110.0, 50.0)]
    out = run("futures", frame(clear)[0])
    assert out["signal"].iloc[-1] == 1 and out["vsb_reason"].iloc[-1] == "entry_long"
    assert_matches_reference(clear, SMALL)


def test_insufficient_history_needs_both_edge_bars():
    params = SMALL
    need = max(params["lookback"], params["vol_period"]) + 2
    rows = flat(need - 2) + [bar(110.0, 50.0)]
    out = run("futures", frame(rows)[0], params)
    assert out["vsb_reason"].iloc[-1] == "insufficient_history" and out["signal"].iloc[-1] == 0
    assert out["vsb_required_history"].iloc[-1] == float(need)
    rows = flat(need - 1) + [bar(110.0, 50.0)]
    out = run("futures", frame(rows)[0], params)
    assert out["vsb_reason"].iloc[-1] == "entry_long"
    assert (out["vsb_reason"].iloc[:need - 1] == "insufficient_history").all()


def test_timestamp_defects_hold_inside_the_dependency_span_only(subtests):
    base = [T0_MS + i * STEP_MS for i in range(14)]
    cases = {
        "duplicate": (lambda ts: ts[:9] + [ts[8]] + ts[10:], "timestamp_order"),
        "reversed": (lambda ts: ts[:9] + [ts[8] - 1] + ts[10:], "timestamp_order"),
        "missing": (lambda ts: ts[:9] + [float("nan")] + ts[10:], "missing_timestamp"),
    }
    rows = flat(12) + [bar(110.0, 50.0), bar(111.0, 60.0)]
    for case, (mutate, reason) in sorted(cases.items()):
        with subtests.test(case=case):
            ts = mutate(list(base))
            df, fts = frame(rows, ts)
            out = run("futures", df)
            assert out["vsb_reason"].iloc[12] == reason and out["signal"].iloc[12] == 0
            assert_matches_reference(rows, SMALL, ts)
    late = flat(3) + [bar(100.0, 10.0)] * 12 + [bar(110.0, 50.0)]
    ts = [T0_MS + i * STEP_MS for i in range(len(late))]
    ts[1] = ts[0]
    out = run("futures", frame(late, ts)[0])
    assert out["vsb_reason"].iloc[-1] == "entry_long" and out["signal"].iloc[-1] == 1


def test_derived_overflow_holds_with_a_reason():
    huge = 1e308
    rows = [bar(100.0, huge) for _ in range(9)] + [bar(110.0, huge)]
    out = run("futures", frame(rows)[0])
    assert out["vsb_reason"].iloc[-1] == "nonfinite_derived" and out["signal"].iloc[-1] == 0
    tiny = [bar(100.0, 1e-300) for _ in range(9)] + [bar(110.0, 1e10)]
    out = run("futures", frame(tiny)[0])
    assert out["vsb_reason"].iloc[-1] == "nonfinite_derived" and out["signal"].iloc[-1] == 0
    assert_matches_reference(rows, SMALL, check_reasons=False)


def test_missing_columns_hold_every_row():
    df, _ = frame(flat(12))
    out = run("futures", df.drop(columns=["volume"]))
    assert (out["vsb_reason"] == "missing_columns").all() and (out["signal"] == 0).all()
