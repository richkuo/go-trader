import math

ATR_PERIOD = 14
MODES = ("retest", "noninverted_retest", "inversion_close")


def _clean_row(row):
    o, h, lo, c = row
    if not all(math.isfinite(x) for x in (o, h, lo, c)):
        return "nonfinite_input"
    if min(o, h, lo, c) <= 0:
        return "nonpositive_price"
    if h < lo or not lo <= o <= h or not lo <= c <= h:
        return "inconsistent_ohlc"
    return ""


def reference(rows, timestamps, params, allow_short, mode="retest"):
    if mode not in MODES:
        raise ValueError(mode)
    mfa = float(params["min_fvg_atr"])
    mga = int(params["max_gap_age"])
    rw = int(params["retest_window"])
    el = float(params["entry_level"])
    support = mga + rw + ATR_PERIOD + 1
    n = len(rows)
    o = [float(r[0]) for r in rows]
    h = [float(r[1]) for r in rows]
    lo = [float(r[2]) for r in rows]
    c = [float(r[3]) for r in rows]

    def row_reason(j):
        reason = _clean_row((o[j], h[j], lo[j], c[j]))
        if reason:
            return reason
        if timestamps is not None and not math.isfinite(timestamps[j]):
            return "missing_timestamp"
        return ""

    def step_bad(j):
        if timestamps is None or j == 0:
            return False
        a, b = timestamps[j - 1], timestamps[j]
        return math.isfinite(a) and math.isfinite(b) and not b > a

    def atr(k):
        if k < ATR_PERIOD:
            return None
        total = 0.0
        for i in range(k - ATR_PERIOD + 1, k + 1):
            pc = c[i - 1]
            total += max(h[i] - lo[i], max(abs(h[i] - pc), abs(lo[i] - pc)))
        return total / float(ATR_PERIOD)

    def gap_at(k):
        if k < 2:
            return None, False
        a = atr(k)
        bull = lo[k] > h[k - 2]
        bear = h[k] < lo[k - 2]
        derived_bad = k >= ATR_PERIOD and (a is None or not math.isfinite(a))
        if not (bull or bear) or a is None or not math.isfinite(a):
            return None, derived_bad
        if bull:
            bottom, top, origin = h[k - 2], lo[k], "bull"
        else:
            bottom, top, origin = h[k], lo[k - 2], "bear"
        width = top - bottom
        threshold = mfa * a
        if not (math.isfinite(width) and math.isfinite(threshold)):
            return None, True
        if not (width > 0.0 and width >= threshold):
            return None, False
        long_level = bottom + el * width
        short_level = top - el * width
        if not (math.isfinite(long_level) and math.isfinite(short_level)):
            return None, True
        return {"k": k, "origin": origin, "bottom": bottom, "top": top, "long_level": long_level,
                "short_level": short_level}, False

    formed = [gap_at(k) for k in range(n)]

    def valid(t):
        start = t - support + 1
        if start < 0:
            return False
        for j in range(start, t + 1):
            if row_reason(j):
                return False
            if j >= start + 1 and step_bad(j):
                return False
        for k in range(start + ATR_PERIOD, t + 1):
            if formed[k][1]:
                return False
        return True

    def invalid_reason(t):
        start = t - support + 1
        for j in range(t, max(start, 0) - 1, -1):
            reason = row_reason(j)
            if reason:
                return reason
            if j >= max(start + 1, 1) and step_bad(j):
                return "timestamp_order"
        if start < 0:
            return "insufficient_history"
        return "nonfinite_derived"

    def zone_from(gap, side, armed_at):
        level = gap["long_level"] if side == "long" else gap["short_level"]
        return {"k": gap["k"], "bottom": gap["bottom"], "top": gap["top"], "level": level, "armed_at": armed_at}

    pending = []
    armed = {"long": None, "short": None}
    signals, reasons = [], []
    for t in range(n):
        cleared = {"long": False, "short": False}
        for side in ("long", "short"):
            z = armed[side]
            if z is None:
                continue
            if t > z["armed_at"] + rw:
                armed[side] = None
            elif (side == "long" and c[t] < z["bottom"]) or (side == "short" and c[t] > z["top"]):
                armed[side] = None
                cleared[side] = True
        armed_now = {"long": False, "short": False}
        if mode != "noninverted_retest":
            hits = {"long": [], "short": []}
            keep = []
            for g in pending:
                if t > g["k"] + mga:
                    continue
                if g["origin"] == "bear" and c[t] > g["top"]:
                    hits["long"].append(g)
                elif g["origin"] == "bull" and c[t] < g["bottom"]:
                    hits["short"].append(g)
                else:
                    keep.append(g)
            pending = keep
            for side in ("long", "short"):
                if hits[side]:
                    best = max(hits[side], key=lambda g: g["k"])
                    armed[side] = zone_from(best, side, t)
                    armed_now[side] = True
                    cleared[side] = False
        gap = formed[t][0]
        if gap is not None:
            if mode == "noninverted_retest":
                side = "long" if gap["origin"] == "bull" else "short"
                armed[side] = zone_from(gap, side, t)
                armed_now[side] = True
                cleared[side] = False
            else:
                pending.append(gap)
        qualified = {"long": False, "short": False}
        if mode == "inversion_close":
            qualified = dict(armed_now)
            armed_now = {"long": False, "short": False}
            for side in ("long", "short"):
                if qualified[side]:
                    armed[side] = None
        else:
            for side in ("long", "short"):
                z = armed[side]
                if z is None or z["armed_at"] >= t:
                    continue
                inside = lo[t] <= z["level"] <= h[t]
                holds = c[t] >= z["bottom"] if side == "long" else c[t] <= z["top"]
                if inside and holds:
                    qualified[side] = True
                    armed[side] = None
        if not valid(t):
            signals.append(0)
            reasons.append(invalid_reason(t))
            continue
        if qualified["long"] and qualified["short"]:
            signals.append(0)
            reasons.append("both_sides_qualified")
        elif qualified["long"]:
            signals.append(1)
            reasons.append("entry_long")
        elif qualified["short"]:
            signals.append(-1 if allow_short else 0)
            reasons.append("entry_short" if allow_short else "short_suppressed_spot")
        elif armed_now["long"] or armed_now["short"]:
            signals.append(0)
            reasons.append("zone_armed_this_bar")
        elif cleared["long"] or cleared["short"]:
            signals.append(0)
            reasons.append("zone_invalidated")
        elif armed["long"] is not None or armed["short"] is not None:
            signals.append(0)
            reasons.append("awaiting_retest")
        else:
            signals.append(0)
            reasons.append("no_armed_zone")
    return signals, reasons
