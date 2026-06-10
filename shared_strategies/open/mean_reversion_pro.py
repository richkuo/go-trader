"""
Mean Reversion Pro — z-score reversion gated by a no-trend filter.

The naive ``mean_reversion`` strategy fades every deviation, which is fatal in
a trend — it keeps buying a falling knife. Mean Reversion Pro only fades a
stretch when (a) there is *no* strong trend (ADX below a ceiling), (b) the
oscillator confirms the extreme (RSI oversold/overbought), and (c) price is
actually turning back toward the mean. The filters that keep it *out* are the
edge.

Rules (long; short is the mirror)
---------------------------------
1. No-trend gate: ADX < ``adx_max`` — never fade a strong directional move.
2. Stretch:       z-score of close vs SMA(lookback) reached <= -``entry_std``.
3. Oscillator:    RSI was oversold (< ``rsi_oversold``) during the stretch.
4. Reversion:     z-score crosses back up through -``entry_std`` (turning up).

Emits ``signal = 1`` (long) / ``-1`` (short) on the reversion bar, else 0.
When used open-as-close, the opposite signal also exits the position.
"""

import numpy as np
import pandas as pd

from adx_trend import _compute_adx_components


def mean_reversion_pro_core(
    df: pd.DataFrame,
    lookback: int = 30,
    entry_std: float = 2.0,
    adx_period: int = 14,
    adx_max: float = 25.0,
    rsi_period: int = 14,
    rsi_oversold: float = 30.0,
    rsi_overbought: float = 70.0,
    confirm_window: int = 3,
) -> pd.DataFrame:
    """Generate trend-filtered mean-reversion signals (bidirectional).

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    lookback : SMA / std window for the z-score
    entry_std : how many std devs of stretch before a reversion is eligible
    adx_period / adx_max : no-trend gate — entries only when ADX < adx_max
    rsi_period : Wilder RSI lookback
    rsi_oversold / rsi_overbought : RSI extremes the stretch must have reached
    confirm_window : bars to look back for the RSI extreme during the stretch

    Returns
    -------
    DataFrame with added columns:
        signal     : 1 (long), -1 (short), 0 (no entry)
        z_score    : (close - SMA) / rolling std
        adx        : Wilder ADX (0 during warmup)
        rsi        : Wilder RSI (NaN during warmup)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(lookback, adx_period * 2, rsi_period) + confirm_window + 2
    if n < min_len:
        result["z_score"] = np.nan
        result["adx"] = 0.0
        result["rsi"] = np.nan
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    rolling_mean = close.rolling(window=lookback).mean()
    rolling_std = close.rolling(window=lookback).std()
    result["z_score"] = (close - rolling_mean) / rolling_std

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    result["adx"] = comps["adx"]

    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))

    z = result["z_score"]
    no_trend = result["adx"] < adx_max

    # Reversion trigger: z crosses back up (long) / down (short) through the band.
    long_revert = (z > -entry_std) & (z.shift(1) <= -entry_std)
    short_revert = (z < entry_std) & (z.shift(1) >= entry_std)

    # Oscillator confirmation: RSI hit the extreme during the recent stretch.
    rsi_was_oversold = (
        (result["rsi"] < rsi_oversold)
        .shift(1)
        .rolling(window=confirm_window)
        .max()
        .fillna(0)
        .astype(bool)
    )
    rsi_was_overbought = (
        (result["rsi"] > rsi_overbought)
        .shift(1)
        .rolling(window=confirm_window)
        .max()
        .fillna(0)
        .astype(bool)
    )

    long_mask = no_trend & long_revert & rsi_was_oversold
    short_mask = no_trend & short_revert & rsi_was_overbought

    result.loc[long_mask, "signal"] = 1
    result.loc[short_mask, "signal"] = -1
    return result
