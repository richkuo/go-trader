import json
import math
import os
import re
import sys
from numbers import Real

import numpy as np
import pandas as pd

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)

from macro_event_inputs import (
    EVENT_TYPES,
    STATUS_SCHEDULED,
    parse_event_records,
    parse_spread_records,
    spread_fraction,
)

STRATEGY = "macro_event_breakout"
TIMESTAMP_KIND_ATTR = "bar_timestamp_kind"
INTERVAL_ATTR = "bar_interval_ms"
DECISION_TIME_ATTR = "decision_time"
NATIVE_CLOSE_KIND = "native_close_inclusive"
SUPPORTED_INTERVALS_MS = (900_000, 3_600_000)
MAX_SUPPORT_ROWS = 200
INJECTED_KEYS = ("event_records", "spread_records")
_HEX64 = re.compile(r"^[0-9a-f]{64}$")

DEFAULT_PARAMS = {
    "event_type": "us_fomc_rate_statement",
    "expected_interval_ms": 3_600_000,
    "range_bars": 6,
    "post_window_bars": 6,
    "confirm_bars": 1,
    "max_spread_frac": 0.0005,
    "max_quote_age_ms": 60_000,
    "cooldown_bars": 24,
    "holding_bars": 12,
    "spread_gate": True,
    "calendar_path": "",
    "calendar_sha256": "",
    "quote_path": "",
    "quote_sha256": "",
}

INT_BOUNDS = {
    "expected_interval_ms": (900_000, 3_600_000),
    "range_bars": (1, 48),
    "post_window_bars": (1, 48),
    "confirm_bars": (1, 6),
    "max_quote_age_ms": (1_000, 3_600_000),
    "cooldown_bars": (0, 96),
    "holding_bars": (1, 240),
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_event": 0,
    "awaiting_confirmation": 2,
    "inside_range": 3,
    "event_consumed": 4,
    "cooldown": 5,
    "bar_not_closed": 6,
    "decision_bar_stale": 7,
    "decision_time_missing": 8,
    "nonfinite_input": 10,
    "inconsistent_ohlc": 11,
    "timestamp_order": 12,
    "missing_columns": 13,
    "cadence_mismatch": 14,
    "calendar_unavailable": 20,
    "calendar_malformed": 21,
    "calendar_not_covered": 22,
    "event_overlap": 23,
    "event_conflict": 24,
    "event_postponed": 25,
    "event_cancelled": 26,
    "event_not_known_before_release": 27,
    "schedule_revised": 28,
    "range_incomplete": 29,
    "confirm_incomplete": 30,
    "quotes_unavailable": 40,
    "quotes_malformed": 41,
    "quote_missing": 42,
    "quote_conflict": 43,
    "spread_too_wide": 44,
}

TEXT_COLUMNS = ("meb_event_id", "meb_event_record_sha256", "meb_calendar_sha256", "meb_side", "meb_quote_sha256",
                "meb_quote_record_sha256", "meb_bid", "meb_ask", "meb_params")
NUMBER_COLUMNS = ("meb_event_revision", "meb_release_ms", "meb_range_start_ms", "meb_range_end_ms", "meb_range_high",
                  "meb_range_low", "meb_window_start_ms", "meb_window_end_ms", "meb_confirm_start_ms",
                  "meb_deadline_ms", "meb_quote_event_ms", "meb_quote_recv_ms", "meb_spread_frac")


def _int_param(name, value):
    lo, hi = INT_BOUNDS[name]
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _identity(name, value, digest):
    if not isinstance(value, str):
        raise ValueError(f"{STRATEGY}: {name} must be a string, got {value!r}")
    if digest and value and not _HEX64.match(value):
        raise ValueError(f"{STRATEGY}: {name} must be a lowercase 64-hex sha256, got {value!r}")
    return value


def support_rows(p: dict) -> int:
    return p["cooldown_bars"] + p["post_window_bars"] + p["range_bars"] + 1


def validate_macro_params(event_type, expected_interval_ms, range_bars, post_window_bars, confirm_bars,
                          max_spread_frac, max_quote_age_ms, cooldown_bars, holding_bars, spread_gate,
                          calendar_path, calendar_sha256, quote_path, quote_sha256) -> dict:
    out = {name: _int_param(name, value) for name, value in (
        ("expected_interval_ms", expected_interval_ms), ("range_bars", range_bars),
        ("post_window_bars", post_window_bars), ("confirm_bars", confirm_bars),
        ("max_quote_age_ms", max_quote_age_ms), ("cooldown_bars", cooldown_bars), ("holding_bars", holding_bars))}
    if event_type not in EVENT_TYPES:
        raise ValueError(f"{STRATEGY}: event_type must be one of {list(EVENT_TYPES)}, got {event_type!r}")
    out["event_type"] = event_type
    if out["expected_interval_ms"] not in SUPPORTED_INTERVALS_MS:
        raise ValueError(f"{STRATEGY}: expected_interval_ms must be one of {list(SUPPORTED_INTERVALS_MS)}, "
                         f"got {expected_interval_ms!r}")
    if isinstance(max_spread_frac, bool) or not isinstance(max_spread_frac, Real):
        raise ValueError(f"{STRATEGY}: max_spread_frac must be a number, got {max_spread_frac!r}")
    spread = float(max_spread_frac)
    if not math.isfinite(spread) or spread <= 0 or spread > 0.01:
        raise ValueError(f"{STRATEGY}: max_spread_frac must be finite and in (0, 0.01], got {max_spread_frac!r}")
    out["max_spread_frac"] = spread
    if not isinstance(spread_gate, (bool, np.bool_)):
        raise ValueError(f"{STRATEGY}: spread_gate must be true or false, got {spread_gate!r}")
    out["spread_gate"] = bool(spread_gate)
    if out["confirm_bars"] > out["post_window_bars"]:
        raise ValueError(f"{STRATEGY}: confirm_bars {out['confirm_bars']} is above post_window_bars "
                         f"{out['post_window_bars']}")
    if out["max_quote_age_ms"] > out["expected_interval_ms"]:
        raise ValueError(f"{STRATEGY}: max_quote_age_ms {out['max_quote_age_ms']} is longer than one "
                         f"{out['expected_interval_ms']} ms bar")
    rows = support_rows(out)
    if rows > MAX_SUPPORT_ROWS:
        raise ValueError(f"{STRATEGY}: cooldown_bars + post_window_bars + range_bars + 1 = {rows} rows exceeds the "
                         f"{MAX_SUPPORT_ROWS}-row check window")
    out["calendar_path"] = _identity("calendar_path", calendar_path, False)
    out["calendar_sha256"] = _identity("calendar_sha256", calendar_sha256, True)
    out["quote_path"] = _identity("quote_path", quote_path, False)
    out["quote_sha256"] = _identity("quote_sha256", quote_sha256, True)
    for path_key, sha_key in (("calendar_path", "calendar_sha256"), ("quote_path", "quote_sha256")):
        if bool(out[path_key]) != bool(out[sha_key]):
            raise ValueError(f"{STRATEGY}: {path_key} and {sha_key} must both be set or both be empty")
    return out


def macro_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - set(INJECTED_KEYS))
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_macro_params(**merged)


def _int_or_none(value):
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, np.integer)):
        return int(value)
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    return None


def _open_times_ms(df: pd.DataFrame, interval: int):
    if "timestamp" in df.columns:
        raw = pd.to_numeric(df["timestamp"], errors="coerce").to_numpy(dtype="float64")
    elif isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        ns_per_unit = {"s": 1_000_000_000, "ms": 1_000_000, "us": 1_000, "ns": 1}[idx.unit]
        missing = np.asarray(idx.isna())
        values = np.where(missing, 0, idx.asi8)
        raw = ((values * ns_per_unit) // 1_000_000).astype("float64")
        raw[missing] = np.nan
    else:
        return None
    kind = df.attrs.get(TIMESTAMP_KIND_ATTR)
    if kind is None:
        finite = raw[np.isfinite(raw)]
        if len(finite) and bool((np.mod(finite, interval) == interval - 1).all()):
            kind = NATIVE_CLOSE_KIND
    if kind == NATIVE_CLOSE_KIND:
        raw = raw - interval + 1
    return raw


def _decision_cutoff(df, calendar, quotes):
    check_time = DECISION_TIME_ATTR in df.attrs
    raw = df.attrs.get(DECISION_TIME_ATTR)
    if isinstance(raw, dict):
        value = raw.get("cutoff_ms")
        if isinstance(value, (int, np.integer)) and not isinstance(value, (bool, np.bool_)) and int(value) > 0:
            return int(value), check_time
    found = [c for c in (calendar.cutoff, quotes.cutoff) if c is not None]
    return (min(found) if found else None), check_time


def _blank(n):
    ev = {col: [""] * n for col in TEXT_COLUMNS}
    ev.update({col: [np.nan] * n for col in NUMBER_COLUMNS})
    return ev


def _finish(result, ev, signal, reason, gate, carried):
    for col in TEXT_COLUMNS:
        result[col] = np.array(ev[col], dtype=object)
    for col in NUMBER_COLUMNS:
        result[col] = np.array(ev[col], dtype="float64")
    result["meb_spread_gate"] = gate
    result["meb_carried"] = carried
    result["meb_reason"] = np.array(reason, dtype=object)
    result["meb_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _empty(result, reason, gate):
    n = len(result)
    return _finish(result, _blank(n), np.zeros(n, dtype=np.int64), [reason] * n,
                   np.full(n, gate, dtype=bool), np.zeros(n, dtype=bool))


def macro_event_breakout_core(
    df: pd.DataFrame,
    event_type: str = "us_fomc_rate_statement",
    expected_interval_ms: int = 3_600_000,
    range_bars: int = 6,
    post_window_bars: int = 6,
    confirm_bars: int = 1,
    max_spread_frac: float = 0.0005,
    max_quote_age_ms: int = 60_000,
    cooldown_bars: int = 24,
    holding_bars: int = 12,
    spread_gate: bool = True,
    calendar_path: str = "",
    calendar_sha256: str = "",
    quote_path: str = "",
    quote_sha256: str = "",
    event_records=None,
    spread_records=None,
) -> pd.DataFrame:
    p = validate_macro_params(event_type, expected_interval_ms, range_bars, post_window_bars, confirm_bars,
                              max_spread_frac, max_quote_age_ms, cooldown_bars, holding_bars, spread_gate,
                              calendar_path, calendar_sha256, quote_path, quote_sha256)
    result = df.copy()
    n = len(result)
    gate = p["spread_gate"]
    for col in ("open", "high", "low", "close"):
        if col not in result.columns:
            return _empty(result, "missing_columns", gate)
    interval = p["expected_interval_ms"]
    declared = result.attrs.get(INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _empty(result, "cadence_mismatch", gate)
    calendar = parse_event_records(event_records)
    if not calendar.error and p["calendar_sha256"] and calendar.sha != p["calendar_sha256"]:
        calendar.error, calendar.detail = "calendar_malformed", (
            f"event_records come from calendar {calendar.sha}, the parameters pin {p['calendar_sha256']}")
    quotes = parse_spread_records(spread_records)
    if not quotes.error and p["quote_sha256"] and quotes.sha != p["quote_sha256"]:
        quotes.error, quotes.detail = "quotes_malformed", (
            f"spread_records come from quotes {quotes.sha}, the parameters pin {p['quote_sha256']}")

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    ts = _open_times_ms(result, interval)
    if ts is None:
        return _empty(result, "timestamp_order", gate)
    finite = np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c) & np.isfinite(ts)
    with np.errstate(invalid="ignore"):
        inconsistent = finite & ((h < lo) | (o > h) | (o < lo) | (c > h) | (c < lo) | (lo <= 0))
    row_reason = [""] * n
    for i in range(n):
        if not finite[i]:
            row_reason[i] = "nonfinite_input"
        elif inconsistent[i]:
            row_reason[i] = "inconsistent_ohlc"
        elif ts[i] % interval:
            row_reason[i] = "timestamp_order"
        elif i > 0 and np.isfinite(ts[i - 1]) and not ts[i] > ts[i - 1]:
            row_reason[i] = "timestamp_order"
    valid_at = {int(ts[i]): i for i in range(n) if not row_reason[i]}
    cutoff, check_time = _decision_cutoff(result, calendar, quotes)
    if check_time and cutoff is None:
        return _empty(result, "decision_time_missing", gate)
    support_ms = support_rows(p) * interval
    post_ms = p["post_window_bars"] * interval
    params_json = json.dumps({k: p[k] for k in sorted(p)}, sort_keys=True, separators=(",", ":"))
    event_ids = sorted(eid for eid, recs in calendar.by_event.items() if recs[0]["event_type"] == p["event_type"])

    def window_start(release_ms):
        return -(-release_ms // interval) * interval

    def in_window(release_ms, t):
        start = window_start(release_ms)
        return start <= t < start + post_ms

    raw = np.zeros(n, dtype=np.int64)
    reason = ["no_event"] * n
    ev = _blank(n)
    consumed = set()
    for k in range(n):
        if row_reason[k]:
            reason[k] = row_reason[k]
            continue
        t = int(ts[k])
        deadline = t + interval
        ev["meb_deadline_ms"][k] = float(deadline)
        if cutoff is not None and deadline > cutoff:
            reason[k] = "bar_not_closed"
            continue
        if calendar.error:
            reason[k] = calendar.error
            continue
        if not calendar.covered(p["event_type"], t - support_ms, deadline):
            reason[k] = "calendar_not_covered"
            continue
        relevant = []
        for eid in event_ids:
            sel = calendar.selected(eid, deadline)
            if sel is None:
                continue
            if sel == "conflict":
                known = [r for r in calendar.by_event[eid] if r["known_ms"] <= deadline]
                if any(in_window(r["scheduled_utc_ms"], t) for r in known):
                    relevant.append((eid, None))
                continue
            if in_window(sel["scheduled_utc_ms"], t):
                relevant.append((eid, sel))
        if not relevant:
            continue
        if len(relevant) > 1:
            reason[k] = "event_overlap"
            ev["meb_event_id"][k] = ",".join(eid for eid, _ in relevant)
            continue
        eid, sel = relevant[0]
        ev["meb_event_id"][k] = eid
        ev["meb_calendar_sha256"][k] = calendar.sha
        ev["meb_params"][k] = params_json
        if sel is None:
            reason[k] = "event_conflict"
            continue
        release = sel["scheduled_utc_ms"]
        ev["meb_event_revision"][k] = float(sel["revision"])
        ev["meb_event_record_sha256"][k] = sel["record_sha256"]
        ev["meb_release_ms"][k] = float(release)
        if sel["status"] != STATUS_SCHEDULED:
            reason[k] = "event_" + sel["status"]
            continue
        at_release = calendar.selected(eid, release)
        if at_release is None:
            reason[k] = "event_not_known_before_release"
            continue
        if at_release == "conflict" or at_release["record_sha256"] != sel["record_sha256"]:
            reason[k] = "schedule_revised"
            continue
        start = window_start(release)
        range_end = (release // interval) * interval
        range_opens = [range_end - m * interval for m in range(p["range_bars"], 0, -1)]
        ev["meb_window_start_ms"][k] = float(start)
        ev["meb_window_end_ms"][k] = float(start + post_ms)
        ev["meb_range_start_ms"][k] = float(range_opens[0])
        ev["meb_range_end_ms"][k] = float(range_end)
        if any(op not in valid_at for op in range_opens):
            reason[k] = "range_incomplete"
            continue
        rows = [valid_at[op] for op in range_opens]
        top = float(max(h[r] for r in rows))
        bottom = float(min(lo[r] for r in rows))
        ev["meb_range_high"][k], ev["meb_range_low"][k] = top, bottom
        first_confirm = t - (p["confirm_bars"] - 1) * interval
        ev["meb_confirm_start_ms"][k] = float(first_confirm)
        if first_confirm < start:
            reason[k] = "awaiting_confirmation"
            continue
        confirm_opens = [first_confirm + m * interval for m in range(p["confirm_bars"])]
        if any(op not in valid_at for op in confirm_opens):
            reason[k] = "confirm_incomplete"
            continue
        closes = [c[valid_at[op]] for op in confirm_opens]
        side = 1 if all(x > top for x in closes) else (-1 if all(x < bottom for x in closes) else 0)
        if side == 0:
            reason[k] = "inside_range"
            continue
        ev["meb_side"][k] = "long" if side > 0 else "short"
        token = (eid, sel["record_sha256"])
        if token in consumed:
            reason[k] = "event_consumed"
            continue
        if gate:
            if quotes.error:
                reason[k] = quotes.error
                continue
            status, quote = quotes.select(deadline, p["max_quote_age_ms"])
            ev["meb_quote_sha256"][k] = quotes.sha
            if quote is not None:
                ev["meb_quote_event_ms"][k] = float(quote["event_ms"])
                ev["meb_quote_recv_ms"][k] = float(quote["recv_ms"])
                ev["meb_bid"][k], ev["meb_ask"][k] = quote["bid"], quote["ask"]
                ev["meb_quote_record_sha256"][k] = quote["record_sha256"]
            if status != "ok":
                reason[k] = status
                continue
            frac = spread_fraction(quote)
            ev["meb_spread_frac"][k] = frac
            if frac > p["max_spread_frac"]:
                reason[k] = "spread_too_wide"
                continue
        raw[k] = side
        consumed.add(token)
        reason[k] = "entry_long" if side > 0 else "entry_short"

    signal = raw.copy()
    entries = [i for i in range(n) if raw[i] != 0]
    for i in entries:
        floor = ts[i] - p["cooldown_bars"] * interval
        if any(j < i and raw[j] != 0 and ts[j] >= floor for j in entries):
            signal[i] = 0
            reason[i] = "cooldown"

    carried = np.zeros(n, dtype=bool)
    if cutoff is not None and n >= 1 and not row_reason[-1]:
        last_deadline = ev["meb_deadline_ms"][-1]
        if reason[-1] == "bar_not_closed" and n >= 2 and not row_reason[-2]:
            prev_deadline = ev["meb_deadline_ms"][-2]
            if 0 <= cutoff - prev_deadline < interval:
                signal[-1] = signal[-2]
                reason[-1] = reason[-2]
                for col in TEXT_COLUMNS + NUMBER_COLUMNS:
                    ev[col][-1] = ev[col][-2]
                carried[-1] = True
        elif reason[-1] != "bar_not_closed" and cutoff - last_deadline >= interval:
            signal[-1] = 0
            reason[-1] = "decision_bar_stale"
    return _finish(result, ev, signal, reason, np.full(n, gate, dtype=bool), carried)
