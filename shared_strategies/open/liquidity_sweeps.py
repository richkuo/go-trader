"""
Liquidity Sweeps (ICT/SMC) — core strategy logic.

Price frequently sweeps beyond key swing highs/lows to hunt stop losses before
reversing. This strategy fades the sweep after confirmation: the candle's wick
pierces the liquidity pool but the body closes back inside the prior range.

Bullish sweep: low dips below recent swing low, close finishes above it → BUY
Bearish sweep: high spikes above recent swing high, close finishes below it → SELL
"""

import numpy as np
import pandas as pd


def _find_swing_highs(highs: pd.Series, lookback: int) -> pd.Series:
    """Mark swing highs — local maxima where high[i] is the max over ±lookback window."""
    swing = pd.Series(np.nan, index=highs.index)
    for i in range(lookback, len(highs) - lookback):
        window = highs.iloc[i - lookback : i + lookback + 1]
        if highs.iloc[i] == window.max():
            swing.iloc[i] = highs.iloc[i]
    return swing


def _find_swing_lows(lows: pd.Series, lookback: int) -> pd.Series:
    """Mark swing lows — local minima where low[i] is the min over ±lookback window."""
    swing = pd.Series(np.nan, index=lows.index)
    for i in range(lookback, len(lows) - lookback):
        window = lows.iloc[i - lookback : i + lookback + 1]
        if lows.iloc[i] == window.min():
            swing.iloc[i] = lows.iloc[i]
    return swing


def liquidity_sweep_core(
    df: pd.DataFrame,
    swing_lookback: int = 20,
    confirmation: int = 1,
) -> pd.DataFrame:
    """
    Detect liquidity sweeps and generate fade signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    swing_lookback : number of candles on each side to identify swing points
    confirmation : reserved for future multi-candle confirmation (currently 1)

    Returns
    -------
    DataFrame with added 'signal' column: 1 (buy), -1 (sell), 0 (hold)
    """
    result = df.copy()
    result["signal"] = 0

    if len(result) < swing_lookback * 2 + 1:
        return result

    swing_highs = _find_swing_highs(result["high"], swing_lookback)
    swing_lows = _find_swing_lows(result["low"], swing_lookback)

    # Track the most recent confirmed swing high/low as liquidity pools
    recent_swing_high = np.nan
    recent_swing_low = np.nan

    for i in range(len(result)):
        high_i = result["high"].iloc[i]
        low_i = result["low"].iloc[i]
        close_i = result["close"].iloc[i]

        # Check for sweeps against current liquidity pools
        if not np.isnan(recent_swing_high):
            # Bearish sweep: wick above swing high but close below it
            if high_i > recent_swing_high and close_i < recent_swing_high:
                if i == 0 or result["signal"].iloc[i - 1] != -1:
                    result.iloc[i, result.columns.get_loc("signal")] = -1
                    recent_swing_high = np.nan  # consumed

        if not np.isnan(recent_swing_low):
            # Bullish sweep: wick below swing low but close above it
            if low_i < recent_swing_low and close_i > recent_swing_low:
                if i == 0 or result["signal"].iloc[i - 1] != 1:
                    result.iloc[i, result.columns.get_loc("signal")] = 1
                    recent_swing_low = np.nan  # consumed

        # Update liquidity pools from confirmed swing points
        # Only use swing points from candles before the current one to avoid lookahead
        if i > 0:
            if not np.isnan(swing_highs.iloc[i - 1]):
                recent_swing_high = swing_highs.iloc[i - 1]
            if not np.isnan(swing_lows.iloc[i - 1]):
                recent_swing_low = swing_lows.iloc[i - 1]

    return result
