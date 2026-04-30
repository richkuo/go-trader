"""
Donchian Channel Breakout — turtle-trading-inspired trend-following strategy.

The Donchian Channel is defined by the highest high and lowest low over a
lookback period.  A breakout occurs when the close crosses above the upper
channel (buy) or below the lower channel (sell).  Only the *crossover* bar
fires a signal — staying above/below the channel does not repeat the signal.

Default parameters follow the classic Turtle system: 20-bar entry channel
and 10-bar exit channel (the exit channel is exposed for downstream use but
the core signal generation uses only the entry channel).
"""

import numpy as np
import pandas as pd


def donchian_breakout_core(
    df: pd.DataFrame,
    entry_period: int = 20,
    exit_period: int = 10,
) -> pd.DataFrame:
    """
    Generate Donchian Channel breakout signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    entry_period : lookback for upper/lower channel (highest high / lowest low)
    exit_period : tighter channel for exits (columns exposed for downstream use,
                  not used for signal generation)

    Returns
    -------
    DataFrame with added columns:
        signal              : 1 (buy), -1 (sell), 0 (hold)
        donchian_upper      : entry upper channel (shifted to avoid lookahead)
        donchian_lower      : entry lower channel (shifted to avoid lookahead)
        donchian_exit_upper : exit upper channel (shifted to avoid lookahead)
        donchian_exit_lower : exit lower channel (shifted to avoid lookahead)
    """
    result = df.copy()
    result["signal"] = 0

    if len(result) < entry_period + 1:
        result["donchian_upper"] = np.nan
        result["donchian_lower"] = np.nan
        result["donchian_exit_upper"] = np.nan
        result["donchian_exit_lower"] = np.nan
        return result

    # Shift by 1 so the channel at bar i is computed from bars [i-entry_period, i-1]
    result["donchian_upper"] = result["high"].rolling(window=entry_period).max().shift(1)
    result["donchian_lower"] = result["low"].rolling(window=entry_period).min().shift(1)

    # Exit channel (tighter, exposed for downstream use)
    result["donchian_exit_upper"] = result["high"].rolling(window=exit_period).max().shift(1)
    result["donchian_exit_lower"] = result["low"].rolling(window=exit_period).min().shift(1)

    close = result["close"]
    upper = result["donchian_upper"]
    lower = result["donchian_lower"]

    prev_close = close.shift(1)
    prev_upper = upper.shift(1)
    prev_lower = lower.shift(1)

    # BUY crossover: close breaks above upper channel
    buy_mask = (close > upper) & (prev_close <= prev_upper)
    # SELL crossover: close breaks below lower channel
    sell_mask = (close < lower) & (prev_close >= prev_lower)

    result.loc[buy_mask, "signal"] = 1
    result.loc[sell_mask, "signal"] = -1

    return result
