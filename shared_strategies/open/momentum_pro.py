import numpy as np
import pandas as pd
from indicators_core import atr_sma_series
from adx_trend import _compute_adx_components

def momentum_pro_core(df: pd.DataFrame, ema_fast: int=20, ema_mid: int=50, ema_long: int=200, adx_period: int=14, adx_threshold: float=20.0, pullback_window: int=6, pullback_touch_buffer_pct: float=0.0, vol_period: int=20, vol_mult: float=1.2, vol_target_atr_pct: float=0.0, vol_target_atr_period: int=14, vol_target_min_fraction: float=0.1) -> pd.DataFrame:
    result = df.copy()
    result['signal'] = 0
    n = len(result)
    min_len = max(ema_long, adx_period * 2, vol_period) + pullback_window + 2
    if n < min_len:
        result['ema_fast'] = result['close'].ewm(span=ema_fast, adjust=False).mean()
        result['ema_mid'] = result['close'].ewm(span=ema_mid, adjust=False).mean()
        result['ema_long'] = result['close'].ewm(span=ema_long, adjust=False).mean()
        result['adx'] = 0.0
        result['vol_sma'] = np.nan
        return result
    close = result['close']
    high = result['high']
    low = result['low']
    volume = result['volume']
    result['ema_fast'] = close.ewm(span=ema_fast, adjust=False).mean()
    result['ema_mid'] = close.ewm(span=ema_mid, adjust=False).mean()
    result['ema_long'] = close.ewm(span=ema_long, adjust=False).mean()
    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    result['adx'] = comps['adx']
    result['vol_sma'] = volume.rolling(window=vol_period).mean()
    ema_fast_s = result['ema_fast']
    ema_mid_s = result['ema_mid']
    ema_long_s = result['ema_long']
    bull_regime = (ema_fast_s > ema_mid_s) & (ema_mid_s > ema_long_s)
    bear_regime = (ema_fast_s < ema_mid_s) & (ema_mid_s < ema_long_s)
    strong_trend = result['adx'] > adx_threshold
    if vol_mult > 0:
        vol_confirm = volume > result['vol_sma'] * vol_mult
    else:
        vol_confirm = pd.Series(True, index=result.index)
    long_touch = low < ema_fast_s * (1.0 - pullback_touch_buffer_pct)
    long_pullback = long_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    short_touch = high > ema_fast_s * (1.0 + pullback_touch_buffer_pct)
    short_pullback = short_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    prev_high = high.shift(1)
    prev_low = low.shift(1)
    long_trigger = (close > ema_fast_s) & (close > prev_high)
    short_trigger = (close < ema_fast_s) & (close < prev_low)
    long_mask = bull_regime & strong_trend & long_pullback & long_trigger & vol_confirm
    short_mask = bear_regime & strong_trend & short_pullback & short_trigger & vol_confirm
    result.loc[long_mask, 'signal'] = 1
    result.loc[short_mask, 'signal'] = -1
    if vol_target_atr_pct > 0:
        atr = atr_sma_series(high, low, close, vol_target_atr_period)
        atr_pct = atr / close
        fraction = (vol_target_atr_pct / atr_pct).clip(lower=vol_target_min_fraction, upper=1.0)
        result['entry_fraction'] = fraction.where(atr_pct > 0)
    return result
