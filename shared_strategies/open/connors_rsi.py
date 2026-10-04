import math
from numbers import Real

import numpy as np
import pandas as pd

WILDER_SPAN = 50
STREAK_CAP = 20
PERIOD_MIN = 2
PERIOD_MAX = 10
RANK_WINDOW_MIN = 20
RANK_WINDOW_MAX = 150
REQUIRED_COLUMNS = ("close",)

REASON_CODES = {
    "entry_long": 1,
    "entry_short": -1,
    "no_cross": 0,
    "below_oversold": 2,
    "above_overbought": 3,
    "insufficient_history": 10,
    "nonfinite_input": 11,
    "nonpositive_close": 12,
    "timestamp_order": 14,
    "missing_columns": 16,
}

_NAME = "connors_rsi_reversion"


def _validate_int(name: str, value, lo: int, hi: int) -> int:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{_NAME}: {name} must be an integer, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or not as_float.is_integer():
        raise ValueError(f"{_NAME}: {name} must be an integer, got {value!r}")
    as_int = int(as_float)
    if as_int < lo or as_int > hi:
        raise ValueError(f"{_NAME}: {name} must be in [{lo}, {hi}], got {value!r}")
    return as_int


def _validate_level(name: str, value) -> float:
    if isinstance(value, bool) or not isinstance(value, Real):
        raise ValueError(f"{_NAME}: {name} must be a number, got {value!r}")
    as_float = float(value)
    if not math.isfinite(as_float) or as_float <= 0.0 or as_float >= 100.0:
        raise ValueError(f"{_NAME}: {name} must be finite and in (0, 100), got {value!r}")
    return as_float


def validate_crsi_params(price_period, streak_period, rank_window, oversold, overbought) -> tuple:
    price_period = _validate_int("price_period", price_period, PERIOD_MIN, PERIOD_MAX)
    streak_period = _validate_int("streak_period", streak_period, PERIOD_MIN, PERIOD_MAX)
    rank_window = _validate_int("rank_window", rank_window, RANK_WINDOW_MIN, RANK_WINDOW_MAX)
    oversold = _validate_level("oversold", oversold)
    overbought = _validate_level("overbought", overbought)
    if not oversold < overbought:
        raise ValueError(
            f"{_NAME}: oversold must be below overbought, got {oversold!r} >= {overbought!r}"
        )
    return price_period, streak_period, rank_window, oversold, overbought


def required_history_bars(rank_window: int) -> int:
    return max(WILDER_SPAN + STREAK_CAP + 1, int(rank_window) + 2) + 1


def wilder_span_weights(period: int, span: int = WILDER_SPAN) -> np.ndarray:
    alpha = 1.0 / period
    decay = 1.0 - alpha
    weights = np.empty(span)
    recursive = span - period
    for lag in range(recursive):
        weights[lag] = alpha * decay ** lag
    seed = decay ** recursive / period
    weights[recursive:] = seed
    return weights


def _shift(values: np.ndarray, lag: int, fill) -> np.ndarray:
    if lag == 0:
        return values.copy()
    out = np.full(len(values), fill, dtype=values.dtype)
    if lag < len(values):
        out[lag:] = values[: len(values) - lag]
    return out


def _window_all(flags: np.ndarray, window: int) -> np.ndarray:
    n = len(flags)
    bad = np.concatenate([[0], np.cumsum(~flags, dtype=np.int64)])
    out = np.zeros(n, dtype=bool)
    if window <= n:
        idx = np.arange(window - 1, n)
        out[idx] = (bad[idx + 1] - bad[idx + 1 - window]) == 0
    return out


def _span_average(values: np.ndarray, weights: np.ndarray) -> np.ndarray:
    acc = np.zeros(len(values))
    for lag, w in enumerate(weights):
        acc = acc + w * _shift(values, lag, 0.0)
    return acc


def _bounded_rsi(change: np.ndarray, change_ok: np.ndarray, period: int) -> tuple:
    safe = np.where(change_ok, change, 0.0)
    gain = np.where(safe > 0, safe, 0.0)
    loss = np.where(safe < 0, -safe, 0.0)
    weights = wilder_span_weights(period)
    avg_gain = _span_average(gain, weights)
    avg_loss = _span_average(loss, weights)
    ok = _window_all(change_ok, WILDER_SPAN)
    total = avg_gain + avg_loss
    rsi = np.full(len(change), np.nan)
    positive = ok & (total > 0)
    rsi[positive] = 100.0 * avg_gain[positive] / total[positive]
    rsi[ok & ~(total > 0)] = 50.0
    return rsi, ok


def _capped_streak(delta: np.ndarray, delta_ok: np.ndarray) -> tuple:
    n = len(delta)
    safe = np.where(delta_ok, delta, 0.0)
    up_alive = np.ones(n, dtype=bool)
    down_alive = np.ones(n, dtype=bool)
    up = np.zeros(n, dtype=np.int64)
    down = np.zeros(n, dtype=np.int64)
    for lag in range(STREAK_CAP):
        step = _shift(safe, lag, 0.0)
        up_alive &= step > 0
        down_alive &= step < 0
        up += up_alive
        down += down_alive
    ok = _window_all(delta_ok, STREAK_CAP)
    streak = np.where(ok, (up - down).astype("float64"), np.nan)
    return streak, ok


def _percent_rank(ret: np.ndarray, ret_ok: np.ndarray, window: int) -> tuple:
    n = len(ret)
    safe = np.where(ret_ok, ret, 0.0)
    below = np.zeros(n, dtype=np.int64)
    equal = np.zeros(n, dtype=np.int64)
    for lag in range(1, window + 1):
        prior = _shift(safe, lag, 0.0)
        below += prior < safe
        equal += prior == safe
    ok = _window_all(ret_ok, window + 1)
    rank = np.full(n, np.nan)
    rank[ok] = 100.0 * (below[ok] + 0.5 * equal[ok]) / window
    return rank, ok


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


def _write(result: pd.DataFrame, cols: dict) -> pd.DataFrame:
    for key, values in cols.items():
        result[key] = values
    return result


def connors_rsi_reversion_core(
    df: pd.DataFrame,
    price_period: int = 3,
    streak_period: int = 2,
    rank_window: int = 100,
    oversold: float = 10.0,
    overbought: float = 90.0,
) -> pd.DataFrame:
    price_period, streak_period, rank_window, oversold, overbought = validate_crsi_params(
        price_period, streak_period, rank_window, oversold, overbought
    )
    result = df.copy()
    n = len(result)
    nan = np.full(n, np.nan)

    missing = [c for c in REQUIRED_COLUMNS if c not in result.columns]
    if missing:
        reason = "missing_columns:" + ",".join(missing)
        return _write(result, {
            "crsi_price_rsi": nan, "crsi_streak": nan, "crsi_streak_rsi": nan,
            "crsi_rank": nan, "crsi": nan, "crsi_prev": nan,
            "crsi_oversold": np.full(n, oversold), "crsi_overbought": np.full(n, overbought),
            "crsi_eval_ts_ms": nan, "crsi_valid": np.zeros(n, dtype=bool),
            "crsi_reason": np.full(n, reason, dtype=object),
            "crsi_reason_code": np.full(n, REASON_CODES["missing_columns"], dtype=np.int64),
            "signal": np.zeros(n, dtype=np.int64),
        })

    close = pd.to_numeric(result["close"], errors="coerce").to_numpy(dtype="float64")
    finite = np.isfinite(close)
    with np.errstate(invalid="ignore"):
        nonpositive = finite & ~(close > 0)

    ts = _timestamps_ms(result)
    bad_ts = np.zeros(n, dtype=bool)
    if ts is not None:
        bad_ts = ~np.isfinite(ts)
        if n > 1:
            with np.errstate(invalid="ignore"):
                bad_ts[1:] |= ~(ts[1:] > ts[:-1])

    row_reason = np.full(n, "", dtype=object)
    row_reason[bad_ts] = "timestamp_order"
    row_reason[nonpositive] = "nonpositive_close"
    row_reason[~finite] = "nonfinite_input"
    row_ok = finite & ~nonpositive & ~bad_ts

    safe_close = np.where(row_ok, close, 1.0)
    prev_close = _shift(safe_close, 1, 1.0)
    delta_ok = row_ok & _shift(row_ok, 1, False)
    delta = np.where(delta_ok, safe_close - prev_close, 0.0)
    ret = np.where(delta_ok, safe_close / prev_close - 1.0, 0.0)

    price_rsi, price_ok = _bounded_rsi(delta, delta_ok, price_period)

    streak, streak_ok = _capped_streak(delta, delta_ok)
    streak_change_ok = streak_ok & _shift(streak_ok, 1, False)
    safe_streak = np.where(streak_ok, streak, 0.0)
    streak_change = np.where(streak_change_ok, safe_streak - _shift(safe_streak, 1, 0.0), 0.0)
    streak_rsi, streak_rsi_ok = _bounded_rsi(streak_change, streak_change_ok, streak_period)

    rank, rank_ok = _percent_rank(ret, delta_ok, rank_window)

    crsi_ok = price_ok & streak_rsi_ok & rank_ok
    crsi = np.full(n, np.nan)
    crsi[crsi_ok] = (price_rsi[crsi_ok] + streak_rsi[crsi_ok] + rank[crsi_ok]) / 3.0
    prev_ok = _shift(crsi_ok, 1, False)
    prev = np.where(prev_ok, _shift(np.where(crsi_ok, crsi, 0.0), 1, 0.0), np.nan)

    decision_ok = crsi_ok & prev_ok
    with np.errstate(invalid="ignore"):
        long_entry = decision_ok & (prev < oversold) & (crsi >= oversold)
        short_entry = decision_ok & (prev > overbought) & (crsi <= overbought)
        below = decision_ok & (crsi < oversold)
        above = decision_ok & (crsi > overbought)

    signal = np.zeros(n, dtype=np.int64)
    signal[long_entry] = 1
    signal[short_entry] = -1

    reason = np.full(n, "no_cross", dtype=object)
    reason[below] = "below_oversold"
    reason[above] = "above_overbought"
    reason[long_entry] = "entry_long"
    reason[short_entry] = "entry_short"

    need = required_history_bars(rank_window)
    for i in np.flatnonzero(~decision_ok):
        if row_reason[i]:
            reason[i] = row_reason[i]
            continue
        lo = max(0, i - need + 1)
        prior = [r for r in row_reason[lo:i] if r]
        reason[i] = prior[-1] if prior else "insufficient_history"

    return _write(result, {
        "crsi_price_rsi": price_rsi,
        "crsi_streak": streak,
        "crsi_streak_rsi": streak_rsi,
        "crsi_rank": rank,
        "crsi": crsi,
        "crsi_prev": prev,
        "crsi_oversold": np.full(n, oversold),
        "crsi_overbought": np.full(n, overbought),
        "crsi_eval_ts_ms": ts if ts is not None else nan,
        "crsi_valid": decision_ok,
        "crsi_reason": reason,
        "crsi_reason_code": np.array([REASON_CODES[r] for r in reason], dtype=np.int64),
        "signal": signal,
    })
