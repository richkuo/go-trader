import math
from numbers import Real

import numpy as np
import pandas as pd

NAME = "inversion_fvg"
PREFIX = "invfvg_"
ATR_PERIOD = 14
LIVE_WINDOW_BARS = 200
WINDOW_MIN = 1
WINDOW_MAX = 200
MIN_FVG_ATR_MAX = 10.0
DEFAULT_PARAMS = {"min_fvg_atr": 0.25, "max_gap_age": 48, "retest_window": 24, "entry_level": 0.5}
PRICE_COLUMNS = ("open", "high", "low", "close")
MODES = ("retest", "noninverted_retest", "inversion_close")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_armed_zone": 0,
    "awaiting_retest": 2,
    "zone_armed_this_bar": 3,
    "zone_invalidated": 4,
    "short_suppressed_spot": 5,
    "both_sides_qualified": 6,
    "insufficient_history": 10,
    "missing_columns": 11,
    "nonfinite_input": 12,
    "nonpositive_price": 13,
    "inconsistent_ohlc": 15,
    "missing_timestamp": 16,
    "timestamp_order": 17,
    "nonfinite_derived": 18,
}

SIDES = ("long", "short")


def _validate_window(name: str, value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < WINDOW_MIN or as_int > WINDOW_MAX:
        raise ValueError(f"{NAME}: {name} must be in [{WINDOW_MIN}, {WINDOW_MAX}], got {value!r}")
    return as_int


def _validate_number(name: str, value, low: float, high: float) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be finite, got {value!r}")
    if as_float < low or as_float > high:
        raise ValueError(f"{NAME}: {name} must be in [{low:g}, {high:g}], got {value!r}")
    return as_float


def required_history_bars(max_gap_age, retest_window) -> int:
    return int(max_gap_age) + int(retest_window) + ATR_PERIOD + 1


def validate_invfvg_params(min_fvg_atr, max_gap_age, retest_window, entry_level) -> tuple:
    min_fvg_atr = _validate_number("min_fvg_atr", min_fvg_atr, 0.0, MIN_FVG_ATR_MAX)
    max_gap_age = _validate_window("max_gap_age", max_gap_age)
    retest_window = _validate_window("retest_window", retest_window)
    entry_level = _validate_number("entry_level", entry_level, 0.0, 1.0)
    support = required_history_bars(max_gap_age, retest_window)
    if support > LIVE_WINDOW_BARS:
        raise ValueError(
            f"{NAME}: max_gap_age + retest_window + {ATR_PERIOD + 1} = {support} exceeds the "
            f"{LIVE_WINDOW_BARS}-bar live decision window (max_gap_age={max_gap_age}, "
            f"retest_window={retest_window})")
    return min_fvg_atr, max_gap_age, retest_window, entry_level


def invfvg_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS))
    if unknown:
        raise ValueError(f"{NAME}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **params}
    validate_invfvg_params(merged["min_fvg_atr"], merged["max_gap_age"], merged["retest_window"],
                           merged["entry_level"])


def _timestamps_ms(df: pd.DataFrame):
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        raw = np.where(missing, 0, idx.asi8)
        values = ((raw * ns_per_unit) // 1_000_000).astype("float64")
        values[missing] = np.nan
        return values
    if "timestamp" in df.columns:
        return pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    return None


def _last_index(flags: np.ndarray) -> np.ndarray:
    idx = np.where(flags, np.arange(len(flags)), -1)
    return np.maximum.accumulate(idx) if len(idx) else idx


def _simple_atr(h: np.ndarray, l: np.ndarray, c: np.ndarray) -> np.ndarray:
    n = len(c)
    out = np.full(n, np.nan)
    if n <= ATR_PERIOD:
        return out
    with np.errstate(invalid="ignore", over="ignore"):
        prev = c[:-1]
        tr = np.maximum(h[1:] - l[1:], np.maximum(np.abs(h[1:] - prev), np.abs(l[1:] - prev)))
        acc = np.zeros(n - ATR_PERIOD)
        for k in range(ATR_PERIOD):
            acc = acc + tr[k:n - ATR_PERIOD + k]
        out[ATR_PERIOD:] = acc / float(ATR_PERIOD)
    return out


class _Zone:
    __slots__ = ("side", "gap_bar", "armed_bar", "bottom", "top", "level", "atr", "event_bar", "event")

    def __init__(self, side, gap_bar, armed_bar, bottom, top, level, atr):
        self.side = side
        self.gap_bar = gap_bar
        self.armed_bar = armed_bar
        self.bottom = bottom
        self.top = top
        self.level = level
        self.atr = atr
        self.event_bar = None
        self.event = None


def _gaps(h, l, c, atr, min_fvg_atr, entry_level, n):
    out = []
    derived_bad = np.zeros(n, dtype=bool)
    derived_bad[ATR_PERIOD:] = ~np.isfinite(atr[ATR_PERIOD:])
    if n < 3:
        return out, derived_bad
    with np.errstate(invalid="ignore", over="ignore"):
        bull = np.zeros(n, dtype=bool)
        bear = np.zeros(n, dtype=bool)
        bull[2:] = l[2:] > h[:-2]
        bear[2:] = h[2:] < l[:-2]
    for k in np.flatnonzero(bull | bear):
        k = int(k)
        a = atr[k]
        if not math.isfinite(a):
            continue
        if bull[k]:
            bottom, top, origin = float(h[k - 2]), float(l[k]), "bull"
        else:
            bottom, top, origin = float(h[k]), float(l[k - 2]), "bear"
        width = top - bottom
        threshold = min_fvg_atr * a
        if not (math.isfinite(width) and math.isfinite(threshold)):
            derived_bad[k] = True
            continue
        if not (width > 0.0 and width >= threshold):
            continue
        long_level = bottom + entry_level * width
        short_level = top - entry_level * width
        if not (math.isfinite(long_level) and math.isfinite(short_level)):
            derived_bad[k] = True
            continue
        out.append((k, origin, bottom, top, long_level, short_level, float(a)))
    return out, derived_bad


def _first_inversion(c, k, origin, bottom, top, max_gap_age, n):
    stop = min(k + max_gap_age, n - 1)
    if stop <= k:
        return None
    window = c[k + 1:stop + 1]
    with np.errstate(invalid="ignore"):
        hits = window > top if origin == "bear" else window < bottom
    if not hits.any():
        return None
    return k + 1 + int(np.argmax(hits))


def _first_event(zone, h, l, c, retest_window, n):
    stop = min(zone.armed_bar + retest_window, n - 1)
    start = zone.armed_bar + 1
    if stop < start:
        return
    hh, ll, cc = h[start:stop + 1], l[start:stop + 1], c[start:stop + 1]
    with np.errstate(invalid="ignore"):
        contained = (ll <= zone.level) & (zone.level <= hh)
        if zone.side == "long":
            cleared = cc < zone.bottom
            qualified = contained & (cc >= zone.bottom)
        else:
            cleared = cc > zone.top
            qualified = contained & (cc <= zone.top)
    events = cleared | qualified
    if not events.any():
        return
    offset = int(np.argmax(events))
    zone.event_bar = start + offset
    zone.event = "cleared" if cleared[offset] else "qualified"


def _zones(gaps, h, l, c, max_gap_age, retest_window, mode, n):
    zones = []
    for k, origin, bottom, top, long_level, short_level, a in gaps:
        if mode == "noninverted_retest":
            side = "long" if origin == "bull" else "short"
            armed = k
        else:
            side = "long" if origin == "bear" else "short"
            armed = _first_inversion(c, k, origin, bottom, top, max_gap_age, n)
            if armed is None:
                continue
        level = long_level if side == "long" else short_level
        zones.append(_Zone(side, k, armed, bottom, top, level, a))
    if mode != "inversion_close":
        for zone in zones:
            _first_event(zone, h, l, c, retest_window, n)
    return zones


def _latest_by_side(zones, n):
    latest = {}
    for side in SIDES:
        ordered = sorted((z for z in zones if z.side == side), key=lambda z: (z.armed_bar, z.gap_bar))
        idx = np.full(n, -1, dtype=np.int64)
        for zi, zone in enumerate(ordered):
            idx[zone.armed_bar] = zi
        if n:
            idx = np.maximum.accumulate(idx)
        latest[side] = (ordered, idx)
    return latest


def _side_state(zone, m, retest_window, mode):
    if zone is None:
        return "none"
    if mode == "inversion_close":
        return "qualified" if zone.armed_bar == m else "none"
    if zone.armed_bar == m:
        return "armed_now"
    if m > zone.armed_bar + retest_window:
        return "none"
    if zone.event_bar is None or zone.event_bar > m:
        return "armed"
    if zone.event_bar == m:
        return zone.event
    return "none"


def _columns(n, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short):
    cols = {}
    for side in SIDES:
        for key in ("zone_bottom", "zone_top", "level", "gap_atr", "gap_ts_ms", "armed_ts_ms",
                    "bars_since_armed", "qualified"):
            cols[f"{PREFIX}{side}_{key}"] = np.full(n, np.nan)
    cols[f"{PREFIX}min_fvg_atr"] = np.full(n, float(min_fvg_atr))
    cols[f"{PREFIX}max_gap_age"] = np.full(n, float(max_gap_age))
    cols[f"{PREFIX}retest_window"] = np.full(n, float(retest_window))
    cols[f"{PREFIX}entry_level"] = np.full(n, float(entry_level))
    cols[f"{PREFIX}required_history"] = np.full(n, float(required_history_bars(max_gap_age, retest_window)))
    cols[f"{PREFIX}short_entries"] = np.full(n, 1.0 if allow_short else 0.0)
    return cols


def _write(result, cols, valid, reason, signal):
    for key, values in cols.items():
        result[key] = values
    result[f"{PREFIX}valid"] = valid
    result[f"{PREFIX}reason"] = reason
    result[f"{PREFIX}reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _evaluate(df: pd.DataFrame, min_fvg_atr, max_gap_age, retest_window, entry_level, *, allow_short: bool,
              mode: str = "retest") -> pd.DataFrame:
    if mode not in MODES:
        raise ValueError(f"{NAME}: mode must be one of {MODES}, got {mode!r}")
    min_fvg_atr, max_gap_age, retest_window, entry_level = validate_invfvg_params(
        min_fvg_atr, max_gap_age, retest_window, entry_level)
    result = df.copy()
    n = len(result)
    cols = _columns(n, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short)
    signal = np.zeros(n, dtype=np.int64)

    if any(col not in result.columns for col in PRICE_COLUMNS):
        return _write(result, cols, np.zeros(n, dtype=bool), np.full(n, "missing_columns", dtype=object), signal)

    o, h, l, c = (pd.to_numeric(result[col], errors="coerce").to_numpy(dtype="float64") for col in PRICE_COLUMNS)
    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(l) & np.isfinite(c)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((o <= 0) | (h <= 0) | (l <= 0) | (c <= 0))
        inconsistent = finite & ((h < l) | (o > h) | (o < l) | (c > h) | (c < l))

    ts = _timestamps_ms(result)
    missing_ts = np.zeros(n, dtype=bool)
    bad_step = np.zeros(n, dtype=bool)
    if ts is not None:
        missing_ts = ~np.isfinite(ts)
        if n > 1:
            both = np.isfinite(ts[1:]) & np.isfinite(ts[:-1])
            with np.errstate(invalid="ignore"):
                bad_step[1:] = both & ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[missing_ts] = "missing_timestamp"
    row_reason[inconsistent] = "inconsistent_ohlc"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_bad = row_reason != ""

    support = required_history_bars(max_gap_age, retest_window)
    atr = _simple_atr(h, l, c)
    gaps, derived_bad = _gaps(h, l, c, atr, min_fvg_atr, entry_level, n)
    zones = _zones(gaps, h, l, c, max_gap_age, retest_window, mode, n)
    latest = _latest_by_side(zones, n)

    last_row_bad = _last_index(row_bad)
    last_step_bad = _last_index(bad_step)
    last_derived_bad = _last_index(derived_bad)
    idx = np.arange(n)
    first = idx - support + 1
    valid = ((first >= 0) & (last_row_bad < first) & (last_step_bad < first + 1)
             & (last_derived_bad < first + ATR_PERIOD))

    reason = np.full(n, "no_armed_zone", dtype=object)
    for m in np.flatnonzero(valid):
        m = int(m)
        states = {}
        shown = {}
        for side in SIDES:
            ordered, pointer = latest[side]
            zi = int(pointer[m])
            zone = ordered[zi] if zi >= 0 else None
            state = _side_state(zone, m, retest_window, mode)
            states[side] = state
            if state in ("armed", "armed_now", "qualified", "cleared"):
                shown[side] = zone
        long_q = states["long"] == "qualified"
        short_q = states["short"] == "qualified"
        if long_q and short_q:
            reason[m] = "both_sides_qualified"
        elif long_q:
            signal[m] = 1
            reason[m] = "entry_long"
        elif short_q:
            if allow_short:
                signal[m] = -1
                reason[m] = "entry_short"
            else:
                reason[m] = "short_suppressed_spot"
        elif "armed_now" in states.values():
            reason[m] = "zone_armed_this_bar"
        elif "cleared" in states.values():
            reason[m] = "zone_invalidated"
        elif "armed" in states.values():
            reason[m] = "awaiting_retest"
        for side in SIDES:
            cols[f"{PREFIX}{side}_qualified"][m] = 1.0 if states[side] == "qualified" else 0.0
            zone = shown.get(side)
            if zone is None:
                continue
            cols[f"{PREFIX}{side}_zone_bottom"][m] = zone.bottom
            cols[f"{PREFIX}{side}_zone_top"][m] = zone.top
            cols[f"{PREFIX}{side}_level"][m] = zone.level
            cols[f"{PREFIX}{side}_gap_atr"][m] = zone.atr
            cols[f"{PREFIX}{side}_bars_since_armed"][m] = float(m - zone.armed_bar)
            if ts is not None:
                cols[f"{PREFIX}{side}_gap_ts_ms"][m] = ts[zone.gap_bar]
                cols[f"{PREFIX}{side}_armed_ts_ms"][m] = ts[zone.armed_bar]

    for i in np.flatnonzero(~valid):
        i = int(i)
        start = i - support + 1
        row_at = int(last_row_bad[i])
        step_at = int(last_step_bad[i])
        row_in = row_at >= max(start, 0)
        step_in = step_at >= max(start + 1, 1)
        if row_in and (not step_in or row_at >= step_at):
            reason[i] = row_reason[row_at]
        elif step_in:
            reason[i] = "timestamp_order"
        elif start < 0:
            reason[i] = "insufficient_history"
        else:
            reason[i] = "nonfinite_derived"

    return _write(result, cols, valid, reason, signal)


def inversion_fvg_core(df: pd.DataFrame, min_fvg_atr=0.25, max_gap_age=48, retest_window=24, entry_level=0.5, *,
                       allow_short: bool) -> pd.DataFrame:
    if not isinstance(allow_short, bool):
        raise ValueError(f"{NAME}: allow_short must be True or False, got {allow_short!r}")
    return _evaluate(df, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short=allow_short)


def inversion_fvg_futures(df: pd.DataFrame, min_fvg_atr=0.25, max_gap_age=48, retest_window=24,
                          entry_level=0.5) -> pd.DataFrame:
    return inversion_fvg_core(df, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short=True)


def inversion_fvg_spot(df: pd.DataFrame, min_fvg_atr=0.25, max_gap_age=48, retest_window=24,
                       entry_level=0.5) -> pd.DataFrame:
    return inversion_fvg_core(df, min_fvg_atr, max_gap_age, retest_window, entry_level, allow_short=False)
