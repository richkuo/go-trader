"""
RSI + Bollinger Bands Combo — mean reversion at Bollinger Band extremes
confirmed by RSI oscillator evidence.

The naive ``bollinger_bands`` strategy fades every band touch, which gets
chopped up when price walks the band. The naive ``rsi`` strategy fires on
any RSI cross without price-context. RSI+BB Combo only enters when both
conditions align: price at a BB extreme *and* RSI confirming the extreme,
then triggers on the reversion cross — higher-quality setups, fewer false
entries.

Rules (long; short is the mirror)
---------------------------------
1. BB stretch: close <= lower band (long) / >= upper band (short).
2. RSI extreme: RSI < oversold (long) / RSI > overbought (short).
3. Reversion: close crosses back above lower band (long) / below upper band
   (short) while RSI remains extreme or was extreme within the recent
   ``confirm_window``.

Emits ``signal = 1`` (long) / ``-1`` (short) on the reversion bar, else 0.
"""

import numpy as np
import pandas as pd

from indicators import sma


def rsi_bb_combo_core(
    df: pd.DataFrame,
    bb_period: int = 20,
    bb_std: float = 2.0,
    rsi_period: int = 14,
    rsi_oversold: float = 30.0,
    rsi_overbought: float = 70.0,
    confirm_window: int = 3,
) -> pd.DataFrame:
    """Generate RSI-confirmed Bollinger Band mean-reversion signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    bb_period : SMA / std lookback for Bollinger Bands
    bb_std : band width in standard deviations
    rsi_period : Wilder RSI lookback
    rsi_oversold / rsi_overbought : RSI extremes required for entry eligibility
    confirm_window : bars to look back for RSI extreme during the BB stretch

    Returns
    -------
    DataFrame with added columns:
        signal     : 1 (long), -1 (short), 0 (no entry)
        bb_middle  : SMA(bb_period)
        bb_upper   : SMA + bb_std * rolling_std
        bb_lower   : SMA - bb_std * rolling_std
        rsi        : Wilder RSI (NaN during warmup)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(bb_period, rsi_period) + confirm_window + 2
    if n < min_len:
        result["bb_middle"] = np.nan
        result["bb_upper"] = np.nan
        result["bb_lower"] = np.nan
        result["rsi"] = np.nan
        return result

    close = result["close"]

    result["bb_middle"] = sma(close, bb_period)
    rolling_std = close.rolling(window=bb_period).std()
    result["bb_upper"] = result["bb_middle"] + (rolling_std * bb_std)
    result["bb_lower"] = result["bb_middle"] - (rolling_std * bb_std)

    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))

    # Reversion trigger: close crosses back up through lower band (long) /
    # back down through upper band (short).
    long_revert = (close > result["bb_lower"]) & (close.shift(1) <= result["bb_lower"].shift(1))
    short_revert = (close < result["bb_upper"]) & (close.shift(1) >= result["bb_upper"].shift(1))

    # RSI confirmation: RSI hit the extreme during the recent stretch.
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

    long_mask = long_revert & rsi_was_oversold
    short_mask = short_revert & rsi_was_overbought

    result.loc[long_mask, "signal"] = 1
    result.loc[short_mask, "signal"] = -1
    return result
