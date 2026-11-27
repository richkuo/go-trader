import math
from decimal import ROUND_HALF_EVEN, Decimal

import pandas as pd

STEP = 300_000
MINUTE = 60_000
HOUR = 3_600_000
DAY = 24 * HOUR
T0 = 1_767_225_600_000
BASE = 100.0
BAR_VOLUME = Decimal("10")

SESSION_LOWS = (-1.2, -1.5, -1.1, -1.25, -1.3, -1.0, -0.9, -0.95)
SESSION_HIGHS = (-0.7, -0.9, -0.6, -0.8, -0.75, -0.4, -0.55, -0.6)
LEVEL_BAR = 18
SWEEP_BAR = 100
CONFIRM_BAR = 101
RETEST_BAR = 103

FLOW_CONTRACT = {
    "source": "hyperliquid_ws_trades", "units": "base", "time_basis": "event_time_receipt_cutoff",
    "identity": "venue+namespace+coin+time+tid", "side_evidence": "hyperliquid-trades-side-aggressor/1638-v1",
    "interval_ms": 60_000, "reorder_grace_ms": 5_000, "max_event_gap_ms": 30_000, "decimal_scale": 8,
}


def _long_template(variant=""):
    bars = []
    prev_c = -0.95
    for hr in range(8):
        lo_h, hi_h = SESSION_LOWS[hr], SESSION_HIGHS[hr]
        mid = (lo_h + hi_h) / 2.0
        for k in range(12):
            o = min(max(prev_c, lo_h + 0.02), hi_h - 0.02)
            c = mid + 0.06 * math.sin(k * 1.3 + hr)
            h = min(max(o, c) + 0.03, hi_h - 0.005)
            lo = max(min(o, c) - 0.03, lo_h + 0.005)
            if k == 3:
                h = hi_h
            if k == 6:
                lo = lo_h
            bars.append([o, h, lo, c])
            prev_c = c
    hour8 = [
        [prev_c, max(prev_c, -0.9) + 0.02, -0.92, -0.9],
        [-0.9, -0.88, -1.0, -0.98],
        [-0.98, -0.98, -1.4, -1.35],
        [-1.35, -1.3, -1.45, -1.4],
        [-1.4, -1.38, -1.7, -1.45],
    ]
    if variant == "weak_displacement":
        hour8 += [[-1.45, -1.0, -1.47, -1.02], [-1.02, -0.97, -1.03, -0.99], [-0.99, -0.95, -1.18, -1.0]]
    else:
        hour8 += [[-1.45, -0.85, -1.47, -0.9], [-0.9, -0.8, -0.95, -0.85], [-0.85, -0.84, -1.18, -1.0]]
    bars.extend(hour8)
    start = len(bars)
    ramp_end = 144
    for i in range(start, ramp_end):
        frac = (i - start + 1) / (ramp_end - start)
        c = -1.0 + 1.0 * frac
        o = bars[-1][3]
        bars.append([o, max(o, c) + 0.03, min(o, c) - 0.03, c])
    for i in range(ramp_end, 288):
        k = i - ramp_end
        c = 0.1 * math.sin(k * 0.7)
        o = bars[-1][3]
        h = min(max(o, c) + 0.05, 0.29)
        lo = max(min(o, c) - 0.05, -0.29)
        if i % 12 == 4:
            h = 0.3
        if i % 12 == 8:
            lo = -0.3
        bars.append([o, h, lo, c])
    flows = [0.5] * 288
    if variant == "new_delta_low":
        flows[SWEEP_BAR] = 0.0
    else:
        flows[LEVEL_BAR] = 0.1
        for i in range(LEVEL_BAR + 1, SWEEP_BAR):
            flows[i] = 0.56
        flows[SWEEP_BAR] = 0.45
    return bars, flows


def _flat_template():
    bars = []
    prev = 0.0
    for i in range(288):
        c = 0.1 * math.sin(i * 0.7)
        h = min(max(prev, c) + 0.05, 0.29)
        lo = max(min(prev, c) - 0.05, -0.29)
        if i % 12 == 4:
            h = 0.3
        if i % 12 == 8:
            lo = -0.3
        bars.append([prev, h, lo, c])
        prev = c
    return bars, [0.5] * 288


def _dec(value: Decimal) -> str:
    text = format(value.normalize(), "f")
    return text.rstrip("0").rstrip(".") if "." in text else text


def build(days, start_ms=T0, variants=None, invalid_minutes=None):
    variants = variants or {}
    invalid_minutes = invalid_minutes or {}
    bars, intervals = [], []
    for d, kind in enumerate(days):
        if kind == "flat":
            template, flows = _flat_template()
            sign = 1.0
        else:
            template, flows = _long_template(variants.get(d, ""))
            sign = 1.0 if kind == "long" else -1.0
        for i, (o, h, lo, c) in enumerate(template):
            if sign < 0:
                o, h, lo, c = -o, -lo, -h, -c
            t = start_ms + d * DAY + i * STEP
            f = flows[i] if sign > 0 else 1.0 - flows[i]
            bars.append({"t": t, "T": t + STEP - 1, "o": round(BASE + o, 6), "h": round(BASE + h, 6),
                         "l": round(BASE + lo, 6), "c": round(BASE + c, 6), "v": float(BAR_VOLUME)})
            per = BAR_VOLUME / (STEP // MINUTE)
            buy = (per * Decimal(repr(f))).quantize(Decimal("0.0001"), rounding=ROUND_HALF_EVEN)
            for k in range(STEP // MINUTE):
                s = t + k * MINUTE
                invalid = invalid_minutes.get(s)
                intervals.append({"start_ms": s, "end_ms": s + MINUTE, "buy": _dec(buy), "sell": _dec(per - buy),
                                  "total": _dec(per), "trades": 4, "records_hash": f"{s:064x}", "session": 1,
                                  "finalized_ms": s + MINUTE + 6_000,
                                  "status": "complete" if invalid is None else "invalid",
                                  "invalid_ms": invalid, "reason": ""})
    return bars, intervals


def flow_payload(intervals, coin="BTC", bar_interval=STEP, cutoff=None, start_ms=None):
    out = {"available": True, "kind": "trade_flow", "coin": coin, **FLOW_CONTRACT, "bar_interval_ms": bar_interval,
           "intervals": [iv for iv in intervals if start_ms is None or iv["start_ms"] >= start_ms], "gaps": []}
    if cutoff is not None:
        out["cutoff_ms"] = int(cutoff)
    return out


def htf_rows(bars, until_ms=None):
    rows = {}
    for b in bars:
        if until_ms is not None and b["T"] + 1 > until_ms:
            break
        key = b["t"] // HOUR * HOUR
        if key not in rows:
            rows[key] = [key, b["o"], b["h"], b["l"], b["c"], b["v"]]
        else:
            r = rows[key]
            r[2] = max(r[2], b["h"])
            r[3] = min(r[3], b["l"])
            r[4] = b["c"]
            r[5] += b["v"]
    return [rows[k] for k in sorted(rows)]


def htf_payload(bars, until_ms=None):
    rows = htf_rows(bars, until_ms)
    complete = [r for r in rows if until_ms is None or r[0] + HOUR <= until_ms]
    return {"available": True, "kind": "htf_candles", "interval_ms": HOUR, "timestamp_kind": "open",
            "rows": complete}


def frame(bars):
    idx = pd.to_datetime([b["t"] for b in bars], unit="ms")
    return pd.DataFrame({"open": [b["o"] for b in bars], "high": [b["h"] for b in bars],
                         "low": [b["l"] for b in bars], "close": [b["c"] for b in bars],
                         "volume": [b["v"] for b in bars]}, index=idx)


def bar_index(day, bar):
    return day * 288 + bar
