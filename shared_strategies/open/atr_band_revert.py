import numpy as np
import pandas as pd
from indicators_core import atr_sma

def atr_band_revert_core(df: pd.DataFrame, period: int=20, atr_period: int=14, k_entry: float=1.5, allow_short: bool=False) -> pd.DataFrame:
    result = df.copy()
    mid = result['close'].rolling(window=period).mean()
    atr = atr_sma(result, atr_period)
    result['atr'] = atr
    result['band_mid'] = mid
    result['band_lower'] = mid - k_entry * atr
    result['band_upper'] = mid + k_entry * atr
    result['signal'] = 0
    long_entry = result['close'] <= result['band_lower']
    result.loc[long_entry.fillna(False), 'signal'] = 1
    if allow_short:
        short_entry = result['close'] >= result['band_upper']
        result.loc[short_entry.fillna(False), 'signal'] = -1
    return result
