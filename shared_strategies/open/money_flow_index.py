import math
from numbers import Real

import numpy as np
import pandas as pd

LOOKBACK_MIN = 2
LOOKBACK_MAX = 198
THRESHOLD_MIN = 0.0
THRESHOLD_MAX = 100.0
ZERO_TOTAL_DIAGNOSTIC = 50.0
REQUIRED_COLUMNS = ("high", "low", "close", "volume")

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "nonpositive_price": 12,
    "inconsistent_hlc": 13,
    "timestamp_order": 14,
    "negative_volume": 15,
    "missing_columns": 16,
    "zero_total_flow": 17,
    "arithmetic_overflow": 18,
}

DIAGNOSTIC_COLUMNS = (
    "mfi_high", "mfi_low", "mfi_close", "mfi_volume", "mfi_typical", "mfi_raw_flow",
    "mfi_positive_flow", "mfi_negative_flow", "mfi_positive_total", "mfi_negative_total",
    "mfi_prev_positive_total", "mfi_prev_negative_total", "mfi_value", "mfi_prev_value",
    "mfi_eval_ts_ms",
)


def _validate_lookback(value) -> int:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"money_flow_index_reversal: lookback must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"money_flow_index_reversal: lookback must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < LOOKBACK_MIN or as_int > LOOKBACK_MAX:
        raise ValueError(
            f"money_flow_index_reversal: lookback must be in [{LOOKBACK_MIN}, {LOOKBACK_MAX}], got {value!r}")
    return as_int


def _validate_threshold(name: str, value) -> float:
    if isinstance(value, (bool, np.bool_)) or not isinstance(value, Real):
        raise ValueError(f"money_flow_index_reversal: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float):
        raise ValueError(f"money_flow_index_reversal: {name} must be finite, got {value!r}")
    return as_float


def validate_mfi_params(lookback, oversold, overbought) -> tuple:
    lookback = _validate_lookback(lookback)
    oversold = _validate_threshold("oversold", oversold)
    overbought = _validate_threshold("overbought", overbought)
    if not (THRESHOLD_MIN <= oversold < overbought <= THRESHOLD_MAX):
        raise ValueError(
            "money_flow_index_reversal: thresholds must satisfy 0 <= oversold < overbought <= 100, "
            f"got oversold={oversold!r} overbought={overbought!r}")
    return lookback, oversold, overbought


def oscillator_history_bars(lookback: int) -> int:
    return int(lookback) + 1


def required_history_bars(lookback: int) -> int:
    return int(lookback) + 2


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


def _exact_total(values) -> float:
    try:
        return math.fsum(values)
    except OverflowError:
        return math.inf


def _oscillator(positive: float, negative: float) -> float:
    if positive > 0.0:
        return 100.0 / (1.0 + negative / positive)
    return 0.0


def _empty_result(result: pd.DataFrame, lookback: int, oversold: float, overbought: float,
                  reason: str) -> pd.DataFrame:
    n = len(result)
    for col in DIAGNOSTIC_COLUMNS:
        result[col] = np.full(n, np.nan)
    result["mfi_lookback"] = np.full(n, float(lookback))
    result["mfi_oversold"] = np.full(n, oversold)
    result["mfi_overbought"] = np.full(n, overbought)
    result["mfi_valid"] = np.zeros(n, dtype=bool)
    result["mfi_reason"] = np.full(n, reason, dtype=object)
    result["mfi_reason_code"] = np.full(n, REASON_CODES["missing_columns"], dtype=np.int64)
    result["signal"] = np.zeros(n, dtype=np.int64)
    return result


def money_flow_index_reversal_core(
    df: pd.DataFrame,
    lookback: int = 14,
    oversold: float = 20.0,
    overbought: float = 80.0,
) -> pd.DataFrame:
    lookback, oversold, overbought = validate_mfi_params(lookback, oversold, overbought)
    result = df.copy()
    n = len(result)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        return _empty_result(result, lookback, oversold, overbought,
                             "missing_columns:" + ",".join(missing))

    high = pd.to_numeric(result["high"], errors="coerce").to_numpy(dtype="float64")
    low = pd.to_numeric(result["low"], errors="coerce").to_numpy(dtype="float64")
    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    volume = pd.to_numeric(result["volume"], errors="coerce").to_numpy(dtype="float64")

    finite = np.isfinite(high) & np.isfinite(low) & np.isfinite(close) & np.isfinite(volume)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ((high <= 0) | (low <= 0) | (close <= 0))
        inconsistent = finite & ~nonpositive & ((high < low) | (close > high) | (close < low))
        negative_volume = finite & ~nonpositive & ~inconsistent & (volume < 0)

    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[negative_volume] = "negative_volume"
    row_reason[inconsistent] = "inconsistent_hlc"
    row_reason[nonpositive] = "nonpositive_price"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~nonpositive & ~inconsistent & ~negative_volume & ~bad_ts

    with np.errstate(over="ignore", invalid="ignore"):
        typical = np.where(row_ok, high / 3.0 + low / 3.0 + close / 3.0, np.nan)
        raw_flow = np.where(row_ok, typical * volume, np.nan)
    flow_overflow = row_ok & ~np.isfinite(raw_flow)

    compare_ok = np.zeros(n, dtype=bool)
    if n > 1:
        compare_ok[1:] = row_ok[1:] & row_ok[:-1]
    positive_flow = np.full(n, np.nan)
    negative_flow = np.full(n, np.nan)
    if n > 1:
        rising = compare_ok[1:] & (typical[1:] > typical[:-1])
        falling = compare_ok[1:] & (typical[1:] < typical[:-1])
        pos = np.where(compare_ok[1:], 0.0, np.nan)
        neg = np.where(compare_ok[1:], 0.0, np.nan)
        pos[rising] = raw_flow[1:][rising]
        neg[falling] = raw_flow[1:][falling]
        positive_flow[1:] = pos
        negative_flow[1:] = neg

    window_ok = np.zeros(n, dtype=bool)
    positive_total = np.full(n, np.nan)
    negative_total = np.full(n, np.nan)
    value = np.full(n, np.nan)
    overflow = np.zeros(n, dtype=bool)
    zero_total = np.zeros(n, dtype=bool)
    osc_ok = np.zeros(n, dtype=bool)
    for i in range(lookback, n):
        lo = i - lookback + 1
        if not compare_ok[lo:i + 1].all():
            continue
        window_ok[i] = True
        if flow_overflow[lo:i + 1].any():
            overflow[i] = True
            continue
        p = _exact_total(positive_flow[lo:i + 1].tolist())
        q = _exact_total(negative_flow[lo:i + 1].tolist())
        if not (math.isfinite(p) and math.isfinite(q)):
            overflow[i] = True
            continue
        positive_total[i] = p
        negative_total[i] = q
        if p == 0.0 and q == 0.0:
            zero_total[i] = True
            value[i] = ZERO_TOTAL_DIAGNOSTIC
            continue
        value[i] = _oscillator(p, q)
        osc_ok[i] = True

    prev_value = np.full(n, np.nan)
    prev_positive_total = np.full(n, np.nan)
    prev_negative_total = np.full(n, np.nan)
    prev_ok = np.zeros(n, dtype=bool)
    prev_zero = np.zeros(n, dtype=bool)
    prev_overflow = np.zeros(n, dtype=bool)
    if n > 1:
        prev_value[1:] = value[:-1]
        prev_positive_total[1:] = positive_total[:-1]
        prev_negative_total[1:] = negative_total[:-1]
        prev_ok[1:] = osc_ok[:-1]
        prev_zero[1:] = zero_total[:-1]
        prev_overflow[1:] = overflow[:-1]

    decision_ok = osc_ok & prev_ok
    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (prev_value <= oversold) & (value > oversold)
        short_entry = decision_ok & (prev_value >= overbought) & (value < overbought)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(lookback)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        if prior:
            reason[i] = prior[-1]
        elif overflow[i] or (prev_overflow[i] and window_ok[i]):
            reason[i] = "arithmetic_overflow"
        elif zero_total[i] or (prev_zero[i] and window_ok[i]):
            reason[i] = "zero_total_flow"
        else:
            reason[i] = "insufficient_history"

    result["mfi_high"] = np.where(row_ok, high, np.nan)
    result["mfi_low"] = np.where(row_ok, low, np.nan)
    result["mfi_close"] = np.where(row_ok, close, np.nan)
    result["mfi_volume"] = np.where(row_ok, volume, np.nan)
    result["mfi_typical"] = typical
    result["mfi_raw_flow"] = np.where(flow_overflow, np.nan, raw_flow)
    result["mfi_positive_flow"] = positive_flow
    result["mfi_negative_flow"] = negative_flow
    result["mfi_positive_total"] = positive_total
    result["mfi_negative_total"] = negative_total
    result["mfi_prev_positive_total"] = prev_positive_total
    result["mfi_prev_negative_total"] = prev_negative_total
    result["mfi_value"] = value
    result["mfi_prev_value"] = prev_value
    result["mfi_eval_ts_ms"] = ts if ts is not None else np.full(n, np.nan)
    result["mfi_lookback"] = np.full(n, float(lookback))
    result["mfi_oversold"] = np.full(n, oversold)
    result["mfi_overbought"] = np.full(n, overbought)
    result["mfi_valid"] = decision_ok
    result["mfi_reason"] = reason
    result["mfi_reason_code"] = np.array([REASON_CODES[r] for r in reason], dtype=np.int64)
    result["signal"] = signal
    return result
