
import numpy as np
import pandas as pd


def donchian_breakout_core(
    df: pd.DataFrame,
    entry_period: int = 20,
    exit_period: int = 10,
) -> pd.DataFrame:
    result = df.copy()
    result["signal"] = 0

    if len(result) < entry_period + 1:
        result["donchian_upper"] = np.nan
        result["donchian_lower"] = np.nan
        result["donchian_exit_upper"] = np.nan
        result["donchian_exit_lower"] = np.nan
        return result

    result["donchian_upper"] = result["high"].rolling(window=entry_period).max().shift(1)
    result["donchian_lower"] = result["low"].rolling(window=entry_period).min().shift(1)

    result["donchian_exit_upper"] = result["high"].rolling(window=exit_period).max().shift(1)
    result["donchian_exit_lower"] = result["low"].rolling(window=exit_period).min().shift(1)

    close = result["close"]
    upper = result["donchian_upper"]
    lower = result["donchian_lower"]

    prev_close = close.shift(1)
    prev_upper = upper.shift(1)
    prev_lower = lower.shift(1)

    buy_mask = (close > upper) & (prev_close <= prev_upper)
    sell_mask = (close < lower) & (prev_close >= prev_lower)

    result.loc[buy_mask, "signal"] = 1
    result.loc[sell_mask, "signal"] = -1

    return result
