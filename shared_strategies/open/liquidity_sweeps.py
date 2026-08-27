
import numpy as np
import pandas as pd


def _find_swing_highs(highs: pd.Series, lookback: int) -> pd.Series:
    swing = pd.Series(np.nan, index=highs.index)
    for i in range(lookback, len(highs) - lookback):
        window = highs.iloc[i - lookback : i + lookback + 1]
        if highs.iloc[i] == window.max():
            swing.iloc[i] = highs.iloc[i]
    return swing


def _find_swing_lows(lows: pd.Series, lookback: int) -> pd.Series:
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
    result = df.copy()
    result["signal"] = 0

    if len(result) < swing_lookback * 2 + 1:
        return result

    swing_highs = _find_swing_highs(result["high"], swing_lookback)
    swing_lows = _find_swing_lows(result["low"], swing_lookback)

    recent_swing_high = np.nan
    recent_swing_low = np.nan

    for i in range(len(result)):
        high_i = result["high"].iloc[i]
        low_i = result["low"].iloc[i]
        close_i = result["close"].iloc[i]

        if not np.isnan(recent_swing_high):
            if high_i > recent_swing_high and close_i < recent_swing_high:
                if i == 0 or result["signal"].iloc[i - 1] != -1:
                    result.iloc[i, result.columns.get_loc("signal")] = -1
                    recent_swing_high = np.nan

        if not np.isnan(recent_swing_low):
            if low_i < recent_swing_low and close_i > recent_swing_low:
                if i == 0 or result["signal"].iloc[i - 1] != 1:
                    result.iloc[i, result.columns.get_loc("signal")] = 1
                    recent_swing_low = np.nan

        confirm_pos = i - swing_lookback - 1
        if confirm_pos >= 0:
            if not np.isnan(swing_highs.iloc[confirm_pos]):
                recent_swing_high = swing_highs.iloc[confirm_pos]
            if not np.isnan(swing_lows.iloc[confirm_pos]):
                recent_swing_low = swing_lows.iloc[confirm_pos]

    return result
