"""Market regime detection for go-trader check scripts.

Computes a 3-state regime label per (symbol, timeframe) from OHLCV data using
Wilder's ADX + directional indicator (+DI/-DI):

  trending_up   — ADX >= threshold AND +DI > -DI
  trending_down — ADX >= threshold AND -DI > +DI
  ranging       — ADX < threshold  (weak or absent trend)

Bars during the ADX warmup window (first 2*period - 1 bars) default to
"ranging" because there is insufficient history for a valid ADX value.

Usage in check scripts (after data fetch and before apply_strategy):

    from regime import latest_regime
    regime_payload = latest_regime(df, period=14, adx_threshold=20.0)
    strategy_params["regime"] = regime_payload
"""

from __future__ import annotations

import numpy as np
import pandas as pd

from atr import standard_atr

_VALID_LABELS = frozenset({"trending_up", "trending_down", "ranging"})
_DEFAULT_METRICS: dict = {"adx": 0.0, "plus_di": 0.0, "minus_di": 0.0, "atr_pct": 0.0}
_DEFAULT_RESULT: dict = {"regime": "ranging", "score": 0.0, "metrics": _DEFAULT_METRICS}


def _adx_components(
    high: np.ndarray,
    low: np.ndarray,
    close: np.ndarray,
    period: int,
) -> dict:
    """Wilder's ADX, +DI, and -DI on numpy arrays.

    Identical algorithm to adx_trend._compute_adx_components; both implement
    the same Wilder smoothing so live and backtest regimes are consistent.
    Returns dict with "plus_di", "minus_di", "adx", "adx_start" arrays/int.
    Bars before adx_start are zero.
    """
    n = len(high)
    tr = np.zeros(n)
    plus_dm = np.zeros(n)
    minus_dm = np.zeros(n)

    for i in range(1, n):
        h_l = high[i] - low[i]
        h_pc = abs(high[i] - close[i - 1])
        l_pc = abs(low[i] - close[i - 1])
        tr[i] = max(h_l, h_pc, l_pc)

        up_move = high[i] - high[i - 1]
        down_move = low[i - 1] - low[i]

        plus_dm[i] = up_move if (up_move > down_move and up_move > 0) else 0.0
        minus_dm[i] = down_move if (down_move > up_move and down_move > 0) else 0.0

    smooth_tr = np.zeros(n)
    smooth_plus_dm = np.zeros(n)
    smooth_minus_dm = np.zeros(n)

    if n > period:
        smooth_tr[period] = np.sum(tr[1 : period + 1])
        smooth_plus_dm[period] = np.sum(plus_dm[1 : period + 1])
        smooth_minus_dm[period] = np.sum(minus_dm[1 : period + 1])

        for i in range(period + 1, n):
            smooth_tr[i] = smooth_tr[i - 1] - smooth_tr[i - 1] / period + tr[i]
            smooth_plus_dm[i] = smooth_plus_dm[i - 1] - smooth_plus_dm[i - 1] / period + plus_dm[i]
            smooth_minus_dm[i] = smooth_minus_dm[i - 1] - smooth_minus_dm[i - 1] / period + minus_dm[i]

    plus_di = np.zeros(n)
    minus_di = np.zeros(n)
    for i in range(period, n):
        if smooth_tr[i] != 0:
            plus_di[i] = 100.0 * smooth_plus_dm[i] / smooth_tr[i]
            minus_di[i] = 100.0 * smooth_minus_dm[i] / smooth_tr[i]

    dx = np.zeros(n)
    for i in range(period, n):
        di_sum = plus_di[i] + minus_di[i]
        if di_sum != 0:
            dx[i] = 100.0 * abs(plus_di[i] - minus_di[i]) / di_sum

    adx = np.zeros(n)
    adx_start = period * 2
    if adx_start >= n:
        return {"plus_di": plus_di, "minus_di": minus_di, "adx": adx, "adx_start": adx_start}

    adx_start = period * 2 - 1
    if adx_start >= n:
        return {"plus_di": plus_di, "minus_di": minus_di, "adx": adx, "adx_start": adx_start}

    adx[adx_start] = np.mean(dx[period : adx_start + 1])
    for i in range(adx_start + 1, n):
        adx[i] = (adx[i - 1] * (period - 1) + dx[i]) / period

    return {"plus_di": plus_di, "minus_di": minus_di, "adx": adx, "adx_start": adx_start}


def compute_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Add regime columns to a copy of df.

    Parameters
    ----------
    df : DataFrame with high, low, close columns
    period : ADX lookback (Wilder's smoothing)
    adx_threshold : ADX value below which the market is considered ranging

    Returns
    -------
    New DataFrame (input not mutated) with extra columns:
        regime       — "trending_up" | "trending_down" | "ranging"
        regime_score — float in [0, 1]; ADX / 100, clamped
        adx          — raw ADX value
        plus_di      — +DI value
        minus_di     — -DI value
    """
    result = df.copy()
    n = len(result)

    result["regime"] = "ranging"
    result["regime_score"] = 0.0
    result["adx"] = 0.0
    result["plus_di"] = 0.0
    result["minus_di"] = 0.0

    if n == 0:
        return result

    components = _adx_components(
        result["high"].values,
        result["low"].values,
        result["close"].values,
        period,
    )
    plus_di = components["plus_di"]
    minus_di = components["minus_di"]
    adx_arr = components["adx"]
    adx_start = components["adx_start"]

    result["adx"] = adx_arr
    result["plus_di"] = plus_di
    result["minus_di"] = minus_di

    for i in range(adx_start, n):
        adx_val = adx_arr[i]
        score = min(adx_val / 100.0, 1.0)
        result.iat[i, result.columns.get_loc("regime_score")] = score

        if adx_val < adx_threshold:
            label = "ranging"
        elif plus_di[i] >= minus_di[i]:
            label = "trending_up"
        else:
            label = "trending_down"
        result.iat[i, result.columns.get_loc("regime")] = label

    return result


def latest_regime(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> dict:
    """Return the current regime from the most recent bar.

    Parameters
    ----------
    df : DataFrame with high, low, close columns (at least 2*period bars
         recommended for a reliable ADX reading)
    period : ADX lookback
    adx_threshold : minimum ADX to call a trend

    Returns
    -------
    dict:
        regime  — "trending_up" | "trending_down" | "ranging"
        score   — float in [0, 1]
        metrics — dict with adx, plus_di, minus_di, atr_pct (all floats)
    """
    if len(df) == 0:
        return {**_DEFAULT_RESULT, "metrics": dict(_DEFAULT_METRICS)}

    reg_df = compute_regime(df, period=period, adx_threshold=adx_threshold)
    last = reg_df.iloc[-1]

    atr_series = standard_atr(df, period=period)
    atr_val = atr_series.iloc[-1] if not atr_series.empty else float("nan")
    try:
        atr_val = float(atr_val)
    except (TypeError, ValueError):
        atr_val = 0.0
    if not (atr_val > 0):
        atr_val = 0.0

    close_val = float(df["close"].iloc[-1])
    atr_pct = (atr_val / close_val * 100.0) if close_val != 0 else 0.0

    return {
        "regime": str(last["regime"]),
        "score": float(last["regime_score"]),
        "metrics": {
            "adx": float(last["adx"]),
            "plus_di": float(last["plus_di"]),
            "minus_di": float(last["minus_di"]),
            "atr_pct": round(atr_pct, 4),
        },
    }


def ensure_regime_columns(
    df: pd.DataFrame,
    period: int = 14,
    adx_threshold: float = 20.0,
) -> pd.DataFrame:
    """Inject regime columns into df in-place, no-op if already present.

    Returns the same DataFrame object (mutations are in-place).
    """
    if "regime" in df.columns:
        return df

    reg_df = compute_regime(df, period=period, adx_threshold=adx_threshold)
    for col in ("regime", "regime_score", "adx", "plus_di", "minus_di"):
        df[col] = reg_df[col].values
    return df
