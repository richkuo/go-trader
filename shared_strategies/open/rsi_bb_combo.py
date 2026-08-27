import numpy as np
import pandas as pd
from indicators_core import wilder_rsi

def rsi_bb_combo_core(df: pd.DataFrame, bb_period: int=20, bb_std: float=2.0, rsi_period: int=14, rsi_oversold: float=30.0, rsi_overbought: float=70.0, confirm_window: int=3) -> pd.DataFrame:
    result = df.copy()
    result['signal'] = 0
    n = len(result)
    min_len = max(bb_period, rsi_period) + confirm_window + 2
    if n < min_len:
        result['bb_middle'] = np.nan
        result['bb_upper'] = np.nan
        result['bb_lower'] = np.nan
        result['rsi'] = np.nan
        return result
    close = result['close']
    result['bb_middle'] = close.rolling(window=bb_period).mean()
    rolling_std = close.rolling(window=bb_period).std()
    result['bb_upper'] = result['bb_middle'] + rolling_std * bb_std
    result['bb_lower'] = result['bb_middle'] - rolling_std * bb_std
    result['rsi'] = wilder_rsi(close, rsi_period)
    long_revert = (close > result['bb_lower']) & (close.shift(1) <= result['bb_lower'].shift(1))
    short_revert = (close < result['bb_upper']) & (close.shift(1) >= result['bb_upper'].shift(1))
    rsi_was_oversold = (result['rsi'] < rsi_oversold).shift(1).rolling(window=confirm_window).max().fillna(0).astype(bool)
    rsi_was_overbought = (result['rsi'] > rsi_overbought).shift(1).rolling(window=confirm_window).max().fillna(0).astype(bool)
    long_mask = long_revert & rsi_was_oversold
    short_mask = short_revert & rsi_was_overbought
    result.loc[long_mask, 'signal'] = 1
    result.loc[short_mask, 'signal'] = -1
    return result
