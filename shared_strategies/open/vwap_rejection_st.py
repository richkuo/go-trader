import numpy as np
import pandas as pd
from indicators_core import wilder_rsi

def _session_vwap(df: pd.DataFrame) -> pd.Series:
    if isinstance(df.index, pd.DatetimeIndex):
        day = df.index.date
    else:
        day = pd.to_datetime(df.index).date
    typical = (df['high'] + df['low'] + df['close']) / 3.0
    tp_vol = typical * df['volume']
    grouped_tp = tp_vol.groupby(day).cumsum()
    grouped_vol = df['volume'].groupby(day).cumsum()
    vwap = grouped_tp / grouped_vol.replace(0, np.nan)
    return vwap.fillna(typical)

def vwap_rejection_st_core(df: pd.DataFrame, ema_short: int=20, ema_mid: int=50, ema_long: int=200, rsi_period: int=14, rsi_max_reclaim: float=50.0, rally_window: int=5, rally_touch_buffer_pct: float=0.001) -> pd.DataFrame:
    result = df.copy()
    result['signal'] = 0
    n = len(result)
    min_len = max(ema_long, rsi_period) + rally_window + 2
    if n < min_len:
        result['ema_short'] = result['close'].ewm(span=ema_short, adjust=False).mean()
        result['ema_mid'] = result['close'].ewm(span=ema_mid, adjust=False).mean()
        result['ema_long'] = result['close'].ewm(span=ema_long, adjust=False).mean()
        result['vwap'] = result['close'] if n == 0 else _session_vwap(result)
        result['rsi'] = np.nan
        return result
    close = result['close']
    open_ = result['open']
    high = result['high']
    result['ema_short'] = close.ewm(span=ema_short, adjust=False).mean()
    result['ema_mid'] = close.ewm(span=ema_mid, adjust=False).mean()
    result['ema_long'] = close.ewm(span=ema_long, adjust=False).mean()
    result['vwap'] = _session_vwap(result)
    result['rsi'] = wilder_rsi(close, rsi_period)
    bearish_regime = result['ema_mid'] < result['ema_long']
    touch_mult = 1.0 + rally_touch_buffer_pct
    rally_touch = (high > result['vwap'] * touch_mult) | (high > result['ema_short'] * touch_mult) | (high > result['ema_mid'] * touch_mult)
    rally_recent = rally_touch.shift(1).rolling(window=rally_window).max().fillna(0).astype(bool)
    rsi_capped = result['rsi'] <= rsi_max_reclaim
    red_bar = close < open_
    below_vwap = close < result['vwap']
    below_ema_short = close < result['ema_short']
    below_ema_mid = close < result['ema_mid']
    trigger = red_bar & below_vwap & below_ema_short & below_ema_mid
    short_mask = bearish_regime & rally_recent & rsi_capped & trigger
    result.loc[short_mask, 'signal'] = -1
    return result
