import math
import os
import sys
from numbers import Real

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

_TOOLS_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "shared_tools"))
if _TOOLS_DIR not in sys.path:
    sys.path.append(_TOOLS_DIR)

from closed_bar import (
    FRAME_INTERVAL_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)

NAME = "regression_channel_reversion"
FIT_WINDOW_MIN = 2
FIT_WINDOW_MAX = 197
DEFAULT_FIT_WINDOW = 50
DEFAULT_BAND_MULTIPLIER = 2.0
DEFAULT_SLOPE_CEILING = 0.10
DEFAULT_MIN_RESIDUAL_STD = 0.0
RESIDUAL_ZERO_REL = 1e-10
REQUIRED_COLUMNS = ("close",)

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_reentry": 0,
    "slope_gate": 2,
    "deviation_gate": 3,
    "insufficient_history": 10,
    "missing_cadence": 11,
    "cadence_gap": 12,
    "timestamp_order": 13,
    "missing_timestamps": 14,
    "nonfinite_input": 15,
    "nonpositive_price": 16,
    "degenerate_fit": 17,
    "nonfinite_derived": 18,
    "missing_columns": 19,
}

DIAGNOSTIC_COLUMNS = (
    "rcr_row_ts_ms",
    "rcr_eval_ts_ms",
    "rcr_prev_eval_ts_ms",
    "rcr_interval_ms",
    "rcr_close",
    "rcr_prev_close",
    "rcr_slope",
    "rcr_intercept",
    "rcr_fit_mean",
    "rcr_resid_std",
    "rcr_slope_norm",
    "rcr_forecast",
    "rcr_lower",
    "rcr_upper",
    "rcr_prev_slope",
    "rcr_prev_intercept",
    "rcr_prev_fit_mean",
    "rcr_prev_resid_std",
    "rcr_prev_slope_norm",
    "rcr_prev_forecast",
    "rcr_prev_lower",
    "rcr_prev_upper",
)


def _validate_fit_window(value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: fit_window must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{NAME}: fit_window must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < FIT_WINDOW_MIN or as_int > FIT_WINDOW_MAX:
        raise ValueError(f"{NAME}: fit_window must be in [{FIT_WINDOW_MIN}, {FIT_WINDOW_MAX}], got {value!r}")
    return as_int


def _validate_number(name: str, value, *, positive: bool) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"{NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"{NAME}: {name} must be finite, got {value!r}")
    if positive and as_float <= 0:
        raise ValueError(f"{NAME}: {name} must be greater than 0, got {value!r}")
    if not positive and as_float < 0:
        raise ValueError(f"{NAME}: {name} must be at least 0, got {value!r}")
    return as_float


def validate_rcr_params(fit_window, band_multiplier, slope_ceiling, min_residual_std) -> tuple:
    return (
        _validate_fit_window(fit_window),
        _validate_number("band_multiplier", band_multiplier, positive=True),
        _validate_number("slope_ceiling", slope_ceiling, positive=False),
        _validate_number("min_residual_std", min_residual_std, positive=False),
    )


def validate_rcr_param_dict(params) -> tuple:
    if params is None:
        params = {}
    if not isinstance(params, dict):
        raise ValueError(f"{NAME}: params must be an object, got {params!r}")
    return validate_rcr_params(
        params.get("fit_window", DEFAULT_FIT_WINDOW),
        params.get("band_multiplier", DEFAULT_BAND_MULTIPLIER),
        params.get("slope_ceiling", DEFAULT_SLOPE_CEILING),
        params.get("min_residual_std", DEFAULT_MIN_RESIDUAL_STD),
    )


def required_history_bars(fit_window: int) -> int:
    return int(fit_window) + 2


def frame_cadence(df: pd.DataFrame) -> tuple:
    raw = df.attrs.get(FRAME_INTERVAL_ATTR)
    interval = None
    if not isinstance(raw, (bool, np.bool_)) and isinstance(raw, (int, np.integer)) and int(raw) > 0:
        interval = int(raw)
    kind = df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR)
    if kind not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        kind = None
    return interval, kind


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


def _window_count(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    out = np.zeros(n, dtype=np.int64)
    if n < window:
        return out
    cs = np.concatenate(([0], np.cumsum(flags.astype(np.int64))))
    out[window - 1:] = cs[window:] - cs[:n - window + 1]
    return out


def _settings(result: pd.DataFrame, fit_window, band_multiplier, slope_ceiling, min_residual_std) -> None:
    n = len(result)
    result["rcr_fit_window"] = np.full(n, fit_window, dtype=np.int64)
    result["rcr_band_multiplier"] = np.full(n, band_multiplier)
    result["rcr_slope_ceiling"] = np.full(n, slope_ceiling)
    result["rcr_min_residual_std"] = np.full(n, min_residual_std)


def _hold_all(result: pd.DataFrame, reason: str, code: str, interval, settings: tuple) -> pd.DataFrame:
    n = len(result)
    for col in DIAGNOSTIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    if interval is not None:
        result["rcr_interval_ms"] = np.full(n, float(interval))
    _settings(result, *settings)
    result["rcr_valid"] = np.zeros(n, dtype=bool)
    result["rcr_reason"] = np.full(n, reason, dtype=object)
    result["rcr_reason_code"] = np.full(n, REASON_CODES[code], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def _last_defect(reasons: np.ndarray, lo: int, hi: int):
    if hi < lo:
        return None
    seg = reasons[lo:hi + 1]
    hits = np.flatnonzero(seg != "")
    if len(hits) == 0:
        return None
    pos = lo + int(hits[-1])
    return pos, reasons[pos]


def _frozen_fits(close: np.ndarray, row_ok: np.ndarray, fit_window: int) -> dict:
    n = len(close)
    out = {key: np.full(n, np.nan) for key in ("slope", "intercept", "mean", "std", "forecast", "slope_norm")}
    if n <= fit_window:
        return out
    safe = np.where(row_ok, close, 1.0)
    windows = sliding_window_view(safe, fit_window)[: n - fit_window]
    x = np.arange(fit_window, dtype="float64") - (fit_window - 1) / 2.0
    sxx = float((x * x).sum())
    with np.errstate(over="ignore", invalid="ignore", divide="ignore"):
        mean = windows.mean(axis=1)
        dev = windows - mean[:, None]
        slope = (dev * x).sum(axis=1) / sxx
        resid = dev - slope[:, None] * x
        std = np.sqrt((resid * resid).sum(axis=1) / fit_window)
        intercept = mean - slope * ((fit_window - 1) / 2.0)
        forecast = mean + slope * ((fit_window + 1) / 2.0)
        slope_norm = np.abs(slope) * (fit_window - 1) / mean
    sl = slice(fit_window, n)
    out["slope"][sl] = slope + 0.0
    out["intercept"][sl] = intercept
    out["mean"][sl] = mean
    out["std"][sl] = std
    out["forecast"][sl] = forecast
    out["slope_norm"][sl] = slope_norm
    return out


def regression_channel_reversion_core(
    df: pd.DataFrame,
    fit_window: int = DEFAULT_FIT_WINDOW,
    band_multiplier: float = DEFAULT_BAND_MULTIPLIER,
    slope_ceiling: float = DEFAULT_SLOPE_CEILING,
    min_residual_std: float = DEFAULT_MIN_RESIDUAL_STD,
) -> pd.DataFrame:
    settings = validate_rcr_params(fit_window, band_multiplier, slope_ceiling, min_residual_std)
    w, k, ceiling, floor = settings
    result = df.copy()
    n = len(result)
    interval, kind = frame_cadence(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _hold_all(result, "missing_columns:" + ",".join(missing), "missing_columns", interval, settings)
    if interval is None:
        return _hold_all(result, "missing_cadence", "missing_cadence", None, settings)
    ts = _timestamps_ms(result)
    if ts is None:
        return _hold_all(result, "missing_timestamps", "missing_timestamps", interval, settings)

    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    ts_ok = np.isfinite(ts)
    close_finite = np.isfinite(close)
    with np.errstate(invalid="ignore"):
        nonpositive = close_finite & (close <= 0)

    row_reason = np.full(n, "", dtype=object)
    row_reason[~ts_ok] = "missing_timestamps"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~close_finite] = "nonfinite_input"
    row_ok = ts_ok & close_finite & ~nonpositive

    edge_ok = np.zeros(n, dtype=bool)
    edge_reason = np.full(n, "", dtype=object)
    if n > 1:
        both = ts_ok[1:] & ts_ok[:-1]
        with np.errstate(invalid="ignore"):
            step = ts[1:] - ts[:-1]
        edge_ok[1:] = both & (step == float(interval))
        tail_reason = np.full(n - 1, "", dtype=object)
        with np.errstate(invalid="ignore"):
            tail_reason[both & (step != float(interval))] = "cadence_gap"
            tail_reason[both & (step <= 0)] = "timestamp_order"
        tail_reason[~both] = "missing_timestamps"
        edge_reason[1:] = tail_reason

    fits = _frozen_fits(close, row_ok, w)
    fit_inputs_ok = np.zeros(n, dtype=bool)
    if n > w:
        rows_full = _window_count(row_ok, w) == w
        edges_full = _window_count(edge_ok, w - 1) == w - 1
        fit_inputs_ok[w:] = rows_full[w - 1:n - 1] & edges_full[w - 1:n - 1]
    derived = np.column_stack([fits[key] for key in ("slope", "intercept", "mean", "std", "forecast", "slope_norm")])
    with np.errstate(invalid="ignore"):
        lower = fits["forecast"] - k * fits["std"]
        upper = fits["forecast"] + k * fits["std"]
    derived_finite = np.isfinite(derived).all(axis=1) & np.isfinite(lower) & np.isfinite(upper)
    with np.errstate(invalid="ignore"):
        degenerate = fit_inputs_ok & derived_finite & ~(fits["std"] > RESIDUAL_ZERO_REL * fits["mean"])
    fit_nonfinite = fit_inputs_ok & ~derived_finite
    fit_ok = fit_inputs_ok & derived_finite & ~degenerate
    cur_ok = fit_ok & row_ok & edge_ok

    prev_ok = np.zeros(n, dtype=bool)
    prev_close = np.full(n, np.nan)
    prev_lower = np.full(n, np.nan)
    prev_upper = np.full(n, np.nan)
    if n > 1:
        prev_ok[1:] = cur_ok[:-1]
        prev_close[1:] = close[:-1]
        prev_lower[1:] = lower[:-1]
        prev_upper[1:] = upper[:-1]
    decision_ok = cur_ok & prev_ok

    with np.errstate(invalid="ignore"):
        long_setup = decision_ok & (prev_close <= prev_lower) & (close > lower) & (close <= upper)
        short_setup = decision_ok & (prev_close >= prev_upper) & (close < upper) & (close >= lower)
        slope_pass = fits["slope_norm"] <= ceiling
        deviation_pass = fits["std"] >= floor
    gate_pass = slope_pass & deviation_pass
    long_entry = long_setup & gate_pass
    short_entry = short_setup & gate_pass

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_reentry", dtype=object)
    setup = long_setup | short_setup
    reason[setup & ~deviation_pass] = "deviation_gate"
    reason[setup & ~slope_pass] = "slope_gate"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(w)
    for i in np.flatnonzero(~decision_ok):
        lo = max(0, i - need + 1)
        found = []
        for hit in (_last_defect(row_reason, lo, i), _last_defect(edge_reason, max(1, lo + 1), i)):
            if hit is not None:
                found.append(hit)
        if found:
            reason[i] = max(found, key=lambda pr: pr[0])[1]
        elif i < need - 1:
            reason[i] = "insufficient_history"
        elif fit_nonfinite[i] or fit_nonfinite[i - 1]:
            reason[i] = "nonfinite_derived"
        elif degenerate[i] or degenerate[i - 1]:
            reason[i] = "degenerate_fit"
        else:
            reason[i] = "insufficient_history"

    eval_ts = np.full(n, np.nan)
    if kind == TIMESTAMP_KIND_OPEN:
        eval_ts = np.where(ts_ok, ts, np.nan)
    elif kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        eval_ts = np.where(ts_ok, ts - float(interval) + 1.0, np.nan)
    prev_eval_ts = np.full(n, np.nan)
    if n > 1:
        prev_eval_ts[1:] = eval_ts[:-1]

    def cur(values):
        return np.where(cur_ok, values, np.nan)

    def prev(values):
        shifted = np.full(n, np.nan)
        if n > 1:
            shifted[1:] = values[:-1]
        return np.where(decision_ok, shifted, np.nan)

    result["rcr_row_ts_ms"] = np.where(ts_ok, ts, np.nan)
    result["rcr_eval_ts_ms"] = eval_ts
    result["rcr_prev_eval_ts_ms"] = np.where(decision_ok, prev_eval_ts, np.nan)
    result["rcr_interval_ms"] = np.full(n, float(interval))
    result["rcr_close"] = np.where(row_ok, close, np.nan)
    result["rcr_prev_close"] = np.where(decision_ok, prev_close, np.nan)
    result["rcr_slope"] = cur(fits["slope"])
    result["rcr_intercept"] = cur(fits["intercept"])
    result["rcr_fit_mean"] = cur(fits["mean"])
    result["rcr_resid_std"] = cur(fits["std"])
    result["rcr_slope_norm"] = cur(fits["slope_norm"])
    result["rcr_forecast"] = cur(fits["forecast"])
    result["rcr_lower"] = cur(lower)
    result["rcr_upper"] = cur(upper)
    result["rcr_prev_slope"] = prev(fits["slope"])
    result["rcr_prev_intercept"] = prev(fits["intercept"])
    result["rcr_prev_fit_mean"] = prev(fits["mean"])
    result["rcr_prev_resid_std"] = prev(fits["std"])
    result["rcr_prev_slope_norm"] = prev(fits["slope_norm"])
    result["rcr_prev_forecast"] = prev(fits["forecast"])
    result["rcr_prev_lower"] = prev(lower)
    result["rcr_prev_upper"] = prev(upper)
    _settings(result, *settings)
    result["rcr_valid"] = decision_ok
    result["rcr_reason"] = reason
    result["rcr_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
