"""
ADX Trend Rider — core strategy logic.

Uses the Average Directional Index (ADX) to measure trend strength and
+DI/-DI crossovers for directional entry signals.

Entry BUY:  ADX > threshold AND +DI crosses above -DI
Entry SELL: ADX > threshold AND -DI crosses above +DI
No signal:  ADX below threshold (weak/no trend)

ADX and directional indicators are computed using Wilder's smoothing method.
"""

import numpy as np
import pandas as pd


def _compute_adx_components(
    high: np.ndarray,
    low: np.ndarray,
    close: np.ndarray,
    period: int,
) -> dict:
    """Compute ADX, +DI, and -DI using Wilder's smoothing.

    Parameters
    ----------
    high, low, close : numpy arrays of the same length n
    period : Wilder smoothing period

    Returns
    -------
    dict with keys:
        "plus_di"  : np.ndarray shape (n,), 0 during warmup
        "minus_di" : np.ndarray shape (n,), 0 during warmup
        "adx"      : np.ndarray shape (n,), 0 during warmup
        "adx_start": int, index from which ADX is first valid
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

        if up_move > down_move and up_move > 0:
            plus_dm[i] = up_move
        else:
            plus_dm[i] = 0.0

        if down_move > up_move and down_move > 0:
            minus_dm[i] = down_move
        else:
            minus_dm[i] = 0.0

    smooth_tr = np.zeros(n)
    smooth_plus_dm = np.zeros(n)
    smooth_minus_dm = np.zeros(n)

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


def adx_trend_core(
    df: pd.DataFrame,
    adx_period: int = 14,
    adx_threshold: float = 25.0,
) -> pd.DataFrame:
    """
    Detect trend-following signals via ADX + DI crossovers.

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    adx_period : lookback period for ADX / DI calculation (Wilder's smoothing)
    adx_threshold : minimum ADX value to confirm a strong trend

    Returns
    -------
    DataFrame with added 'signal' column: 1 (buy), -1 (sell), 0 (hold)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    if n < adx_period * 2 + 1:
        return result

    components = _compute_adx_components(
        result["high"].values,
        result["low"].values,
        result["close"].values,
        adx_period,
    )
    plus_di = components["plus_di"]
    minus_di = components["minus_di"]
    adx = components["adx"]
    adx_start = components["adx_start"]

    if adx_start >= n:
        return result

    sig_col = result.columns.get_loc("signal")
    for i in range(adx_start + 1, n):
        if adx[i] <= adx_threshold:
            continue

        prev_bull = plus_di[i - 1] > minus_di[i - 1]
        curr_bull = plus_di[i] > minus_di[i]

        if curr_bull and not prev_bull:
            result.iloc[i, sig_col] = 1
        elif not curr_bull and prev_bull:
            result.iloc[i, sig_col] = -1

    return result
