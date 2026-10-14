import json
import math
import os
import sys
from fractions import Fraction
from numbers import Real

import numpy as np
import pandas as pd

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)

from volume_profile_inputs import (
    SESSION_MS,
    ProfileInputError,
    parse_profile_input,
    usable_artifact,
    value_area,
)

STRATEGY = "volume_profile_reclaim"
TIMESTAMP_KIND_ATTR = "bar_timestamp_kind"
INTERVAL_ATTR = "bar_interval_ms"
DECISION_TIME_ATTR = "decision_time"
NATIVE_CLOSE_KIND = "native_close_inclusive"
INJECTED_KEY = "volume_profile_observations"
MAX_PROFILE_BINS = 512
SUPPORTED_INTERVALS_MS = (300_000, 900_000, 1_800_000, 3_600_000)

DEFAULT_PARAMS = {
    "profile_bins": 64,
    "value_area_fraction": 0.70,
    "session_bounds": False,
    "expected_interval_ms": 900_000,
}

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "inside_value_area": 0,
    "outside_value_area": 2,
    "boundary_touch": 3,
    "opposite_boundary_overshoot": 4,
    "session_start": 5,
    "bar_gap": 6,
    "bar_not_closed": 7,
    "decision_bar_stale": 8,
    "decision_time_missing": 9,
    "nonfinite_input": 10,
    "inconsistent_ohlc": 11,
    "timestamp_order": 12,
    "missing_columns": 13,
    "cadence_mismatch": 14,
    "profile_input_missing": 20,
    "unsupported_profile": 21,
    "profile_malformed": 22,
    "profile_unavailable": 23,
    "profile_pending": 24,
    "profile_invalidated": 25,
    "profile_changed": 26,
}

TEXT_COLUMNS = ("vpr_side", "vpr_profile_sha256", "vpr_trades_hash", "vpr_profile_status", "vpr_val", "vpr_vah",
                "vpr_poc_low", "vpr_poc_high", "vpr_bin_width", "vpr_profile_low", "vpr_profile_high", "vpr_bins_json",
                "vpr_params")
NUMBER_COLUMNS = ("vpr_session_ms", "vpr_profile_session_ms", "vpr_available_ms", "vpr_invalid_ms", "vpr_bins",
                  "vpr_total_volume", "vpr_va_volume", "vpr_poc_volume", "vpr_prev_open_ms", "vpr_prev_close",
                  "vpr_close", "vpr_prev_deadline_ms", "vpr_deadline_ms", "vpr_val_f", "vpr_vah_f")


def _int_param(name, value, lo, hi):
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{STRATEGY}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{STRATEGY}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def validate_vpr_params(profile_bins, value_area_fraction, session_bounds, expected_interval_ms) -> dict:
    out = {"profile_bins": _int_param("profile_bins", profile_bins, 2, MAX_PROFILE_BINS)}
    if isinstance(value_area_fraction, bool) or not isinstance(value_area_fraction, Real):
        raise ValueError(f"{STRATEGY}: value_area_fraction must be a number, got {value_area_fraction!r}")
    frac = float(value_area_fraction)
    if not math.isfinite(frac) or not 0 < frac < 1:
        raise ValueError(f"{STRATEGY}: value_area_fraction must be finite and strictly between 0 and 1, got {value_area_fraction!r}")
    out["value_area_fraction"] = frac
    if not isinstance(session_bounds, (bool, np.bool_)):
        raise ValueError(f"{STRATEGY}: session_bounds must be true or false, got {session_bounds!r}")
    out["session_bounds"] = bool(session_bounds)
    interval = _int_param("expected_interval_ms", expected_interval_ms, min(SUPPORTED_INTERVALS_MS), max(SUPPORTED_INTERVALS_MS))
    if interval not in SUPPORTED_INTERVALS_MS:
        raise ValueError(f"{STRATEGY}: expected_interval_ms must be one of {SUPPORTED_INTERVALS_MS}, got {expected_interval_ms!r}")
    out["expected_interval_ms"] = interval
    return out


def vpr_param_validator(params: dict) -> None:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{STRATEGY}: params must be an object, got {params!r}")
    unknown = sorted(set(params) - set(DEFAULT_PARAMS) - {INJECTED_KEY})
    if unknown:
        raise ValueError(f"{STRATEGY}: unknown parameters {unknown}; expected a subset of {sorted(DEFAULT_PARAMS)}")
    merged = {**DEFAULT_PARAMS, **{k: v for k, v in params.items() if k in DEFAULT_PARAMS}}
    validate_vpr_params(**merged)


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


def _decision_cutoff(df, profile):
    check_time = DECISION_TIME_ATTR in df.attrs
    raw = df.attrs.get(DECISION_TIME_ATTR)
    if isinstance(raw, dict):
        value = raw.get("cutoff_ms")
        if isinstance(value, (int, np.integer)) and not isinstance(value, (bool, np.bool_)) and int(value) > 0:
            return int(value), check_time
    return profile.cutoff, check_time


def _blank(n):
    ev = {col: [""] * n for col in TEXT_COLUMNS}
    ev.update({col: [np.nan] * n for col in NUMBER_COLUMNS})
    return ev


def _finish(result, ev, signal, reason, carried):
    for col in TEXT_COLUMNS:
        result[col] = np.array(ev[col], dtype=object)
    for col in NUMBER_COLUMNS:
        result[col] = np.array(ev[col], dtype="float64")
    result["vpr_carried"] = carried
    result["vpr_reason"] = np.array(reason, dtype=object)
    result["vpr_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result


def _empty(result, reason):
    n = len(result)
    return _finish(result, _blank(n), np.zeros(n, dtype=np.int64), [reason] * n, np.zeros(n, dtype=bool))


def _frac_text(x: Fraction) -> str:
    if x.denominator == 1:
        return str(x.numerator)
    return f"{x.numerator}/{x.denominator}"


def volume_profile_reclaim_core(
    df: pd.DataFrame,
    profile_bins: int = 64,
    value_area_fraction: float = 0.70,
    session_bounds: bool = False,
    expected_interval_ms: int = 900_000,
    volume_profile_observations=None,
) -> pd.DataFrame:
    p = validate_vpr_params(profile_bins, value_area_fraction, session_bounds, expected_interval_ms)
    result = df.copy()
    n = len(result)
    for col in ("open", "high", "low", "close"):
        if col not in result.columns:
            return _empty(result, "missing_columns")
    interval = p["expected_interval_ms"]
    declared = result.attrs.get(INTERVAL_ATTR)
    if declared is not None and _int_or_none(declared) != interval:
        return _empty(result, "cadence_mismatch")
    profile = parse_profile_input(volume_profile_observations)
    if not profile.error and profile.bar_interval is not None and profile.bar_interval != interval:
        profile.error, profile.detail = "unsupported_profile", (
            f"bar_interval_ms {profile.bar_interval} differs from expected_interval_ms {interval}")

    o = pd.to_numeric(result["open"], errors="coerce").to_numpy(dtype="float64")
    h = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    lo = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    c = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    ts = _open_times_ms(result, interval)
    if ts is None:
        return _empty(result, "timestamp_order")
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
    cutoff, check_time = _decision_cutoff(result, profile)
    if check_time and cutoff is None:
        return _empty(result, profile.error or "decision_time_missing")
    fraction = Fraction(repr(p["value_area_fraction"]))
    params_json = json.dumps({k: p[k] for k in sorted(p)}, sort_keys=True, separators=(",", ":"))
    areas = {}

    def area_for(art):
        key = art["sha256"]
        if key not in areas:
            areas[key] = value_area(art, p["profile_bins"], fraction, p["session_bounds"])
        return areas[key]

    signal = np.zeros(n, dtype=np.int64)
    reason = ["inside_value_area"] * n
    ev = _blank(n)
    for k in range(n):
        if row_reason[k]:
            reason[k] = row_reason[k]
            continue
        t = int(ts[k])
        deadline = t + interval
        session = t - t % SESSION_MS
        ev["vpr_deadline_ms"][k] = float(deadline)
        ev["vpr_session_ms"][k] = float(session)
        ev["vpr_close"][k] = float(c[k])
        ev["vpr_params"][k] = params_json
        if cutoff is not None and deadline > cutoff:
            reason[k] = "bar_not_closed"
            continue
        if profile.error:
            reason[k] = profile.error
            ev["vpr_profile_status"][k] = profile.detail
            continue
        prior = session - SESSION_MS
        ev["vpr_profile_session_ms"][k] = float(prior)
        entry = profile.lookup(prior)
        art = usable_artifact(entry, deadline)
        if art is None:
            if entry is None:
                reason[k] = "profile_unavailable"
                ev["vpr_profile_status"][k] = f"no record for session {prior}"
            elif entry["artifact"] is not None and entry["invalid_ms"] is not None and entry["invalid_ms"] <= deadline:
                reason[k] = "profile_invalidated"
                ev["vpr_profile_status"][k] = f"{entry['code']}: {entry['reason']}"
                ev["vpr_invalid_ms"][k] = float(entry["invalid_ms"])
            elif entry["artifact"] is not None or entry["status"] == "pending":
                reason[k] = "profile_pending"
                ev["vpr_profile_status"][k] = f"{entry['code']}: {entry['reason']}" if entry["code"] else "not available by this deadline"
            else:
                reason[k] = "profile_unavailable"
                ev["vpr_profile_status"][k] = f"{entry['code']}: {entry['reason']}"
            continue
        ev["vpr_profile_sha256"][k] = art["sha256"]
        ev["vpr_trades_hash"][k] = art["trades_hash"]
        ev["vpr_available_ms"][k] = float(art["available_ms"])
        if entry["invalid_ms"] is not None:
            ev["vpr_invalid_ms"][k] = float(entry["invalid_ms"])
        try:
            area = area_for(art)
        except ProfileInputError as exc:
            reason[k] = "profile_malformed"
            ev["vpr_profile_status"][k] = str(exc)
            continue
        ev["vpr_val"][k], ev["vpr_vah"][k] = _frac_text(area["val"]), _frac_text(area["vah"])
        ev["vpr_val_f"][k], ev["vpr_vah_f"][k] = float(area["val"]), float(area["vah"])
        ev["vpr_poc_low"][k], ev["vpr_poc_high"][k] = _frac_text(area["poc_low"]), _frac_text(area["poc_high"])
        ev["vpr_bin_width"][k] = _frac_text(area["width"])
        ev["vpr_profile_low"][k], ev["vpr_profile_high"][k] = _frac_text(area["low"]), _frac_text(area["high"])
        ev["vpr_bins"][k] = float(area["bins"])
        ev["vpr_total_volume"][k] = area["total"] / 1e8
        ev["vpr_va_volume"][k] = area["va_volume"] / 1e8
        ev["vpr_poc_volume"][k] = area["volume"][area["poc"]] / 1e8
        if t == session:
            reason[k] = "session_start"
            continue
        j = k - 1
        if j < 0 or row_reason[j] or int(ts[j]) != t - interval:
            reason[k] = "bar_gap"
            continue
        prev_deadline = t
        ev["vpr_prev_open_ms"][k] = float(t - interval)
        ev["vpr_prev_deadline_ms"][k] = float(prev_deadline)
        ev["vpr_prev_close"][k] = float(c[j])
        if usable_artifact(entry, prev_deadline) is None:
            reason[k] = "profile_changed"
            continue
        prev_close, close = Fraction(float(c[j])), Fraction(float(c[k]))
        val, vah = area["val"], area["vah"]
        inside = val < close < vah
        if prev_close < val:
            if inside:
                signal[k], reason[k] = 1, "entry_long"
            elif close >= vah:
                reason[k] = "opposite_boundary_overshoot"
            elif close == val:
                reason[k] = "boundary_touch"
            else:
                reason[k] = "outside_value_area"
        elif prev_close > vah:
            if inside:
                signal[k], reason[k] = -1, "entry_short"
            elif close <= val:
                reason[k] = "opposite_boundary_overshoot"
            elif close == vah:
                reason[k] = "boundary_touch"
            else:
                reason[k] = "outside_value_area"
        elif inside:
            reason[k] = "inside_value_area"
        elif close == val or close == vah:
            reason[k] = "boundary_touch"
        else:
            reason[k] = "outside_value_area"
        if signal[k] != 0:
            ev["vpr_side"][k] = "long" if signal[k] > 0 else "short"
            ev["vpr_bins_json"][k] = json.dumps(area["volume"], separators=(",", ":"))

    carried = np.zeros(n, dtype=bool)
    if cutoff is not None and n >= 1 and not row_reason[-1]:
        last_deadline = ev["vpr_deadline_ms"][-1]
        if reason[-1] == "bar_not_closed" and n >= 2 and not row_reason[-2]:
            prev_deadline = ev["vpr_deadline_ms"][-2]
            if 0 <= cutoff - prev_deadline < interval:
                signal[-1] = signal[-2]
                reason[-1] = reason[-2]
                for col in TEXT_COLUMNS + NUMBER_COLUMNS:
                    ev[col][-1] = ev[col][-2]
                carried[-1] = True
        elif reason[-1] != "bar_not_closed" and cutoff - last_deadline >= interval:
            signal[-1] = 0
            reason[-1] = "decision_bar_stale"
    return _finish(result, ev, signal, reason, carried)
