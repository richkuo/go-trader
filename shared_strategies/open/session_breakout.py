import numpy as np
import pandas as pd
from indicators_core import atr_from_true_range, true_range
SESSION_WINDOWS = {'asian': (0, 8), 'us_open': (13, 15), 'us_close': (20, 21)}

def session_breakout_core(df: pd.DataFrame, session: str='asian', lookback: int=1, volume_threshold: float=1.5, vol_period: int=20, atr_period: int=14, atr_multiplier: float=0.0) -> pd.DataFrame:
    result = df.copy()
    result['signal'] = 0
    result['session_high'] = np.nan
    result['session_low'] = np.nan
    result['vol_sma'] = np.nan
    if not isinstance(result.index, pd.DatetimeIndex) or result.empty:
        return result
    if session not in SESSION_WINDOWS:
        raise ValueError(f'Unknown session {session!r}. Valid values: {list(SESSION_WINDOWS)}')
    start_hour, end_hour = SESSION_WINDOWS[session]
    hours = result.index.hour
    in_session = (hours >= start_hour) & (hours < end_hour)
    dates = result.index.normalize()
    session_bars = result[in_session]
    sess_df = session_bars.groupby(session_bars.index.normalize()).agg(s_high=('high', 'max'), s_low=('low', 'min'))
    if sess_df.empty:
        return result
    sess_df['level_high'] = sess_df['s_high'].rolling(window=lookback, min_periods=1).max()
    sess_df['level_low'] = sess_df['s_low'].rolling(window=lookback, min_periods=1).min()
    level_high_by_day = sess_df['level_high']
    level_low_by_day = sess_df['level_low']
    result['session_high'] = dates.to_series(index=result.index).map(level_high_by_day)
    result['session_low'] = dates.to_series(index=result.index).map(level_low_by_day)
    result['vol_sma'] = result['volume'].rolling(window=vol_period, min_periods=vol_period).mean()
    high_volume = result['volume'] > result['vol_sma'] * volume_threshold
    if atr_multiplier > 0:
        tr = true_range(result)
        atr = atr_from_true_range(tr, atr_period, round_large=False, min_periods=atr_period)
        atr_ok = tr > atr * atr_multiplier
    else:
        atr_ok = pd.Series(True, index=result.index)
    after_session = hours >= end_hour
    level_high = result['session_high']
    level_low = result['session_low']
    break_up = (result['close'] > level_high) & high_volume & atr_ok & after_session
    break_down = (result['close'] < level_low) & high_volume & atr_ok & after_session
    result.loc[break_up & ~break_up.shift(1, fill_value=False), 'signal'] = 1
    result.loc[break_down & ~break_down.shift(1, fill_value=False), 'signal'] = -1
    return result
