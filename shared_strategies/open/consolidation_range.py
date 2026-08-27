import numpy as np
import pandas as pd

def consolidation_range_core(df: pd.DataFrame, box_width_pct: float=0.05, min_bars: int=16, edge_entry_frac: float=0.2) -> pd.DataFrame:
    result = df.copy()
    roll_hi = result['high'].rolling(window=min_bars).max()
    roll_lo = result['low'].rolling(window=min_bars).min()
    mid = (roll_hi + roll_lo) / 2.0
    height = roll_hi - roll_lo
    safe_mid = mid.replace(0, np.nan)
    safe_height = height.replace(0, np.nan)
    width = height / safe_mid
    pos = (result['close'] - roll_lo) / safe_height
    result['box_top'] = roll_hi
    result['box_bottom'] = roll_lo
    result['box_mid'] = mid
    result['in_range'] = (width <= box_width_pct).fillna(False)
    result['signal'] = 0
    long_entry = result['in_range'] & (pos <= edge_entry_frac)
    short_entry = result['in_range'] & (pos >= 1 - edge_entry_frac)
    result.loc[long_entry, 'signal'] = 1
    result.loc[short_entry, 'signal'] = -1
    return result
