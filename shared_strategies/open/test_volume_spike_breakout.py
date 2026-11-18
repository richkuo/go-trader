import math
import os
import sys

import numpy as np
import pandas as pd

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
