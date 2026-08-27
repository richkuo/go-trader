import numpy as np
import pandas as pd
from indicators_core import atr_sma_series
from adx_trend import _compute_adx_components
from mtf_confluence import _project_to_native, _resample_htf
_COMPOSITE_ADX_PERIOD_CAP = 14
_WARMUP = 0
_TREND_UP_CLEAN = 1
_TREND_UP_CHOPPY = 2
_TREND_DOWN_CLEAN = -1
_TREND_DOWN_CHOPPY = -2
_RANGING_DIRECTIONAL = 3
_RANGING_VOLATILE = 4
_RANGING_QUIET = 5
_LABEL_NAMES = {_WARMUP: '', _TREND_UP_CLEAN: 'trending_up_clean', _TREND_UP_CHOPPY: 'trending_up_choppy', _TREND_DOWN_CLEAN: 'trending_down_clean', _TREND_DOWN_CHOPPY: 'trending_down_choppy', _RANGING_DIRECTIONAL: 'ranging_directional', _RANGING_VOLATILE: 'ranging_volatile', _RANGING_QUIET: 'ranging_quiet'}
_UP_FAMILY = (_TREND_UP_CLEAN, _TREND_UP_CHOPPY)
_DOWN_FAMILY = (_TREND_DOWN_CLEAN, _TREND_DOWN_CHOPPY)
_MR_FAMILY = (_RANGING_QUIET, _RANGING_VOLATILE, _TREND_UP_CHOPPY, _TREND_DOWN_CHOPPY)

def _classify_buckets(htf: pd.DataFrame, period: int, adx_threshold: float, return_eff_threshold: float, range_eff_threshold: float, efficiency_threshold: float) -> np.ndarray:
    close = htf['close']
    high = htf['high']
    low = htf['low']
    atr = atr_sma_series(high, low, close, period)
    denom = atr * period
    net = close - close.shift(period - 1)
    return_eff = (net / denom).where(denom > 0, 0.0)
    range_eff = ((high.rolling(window=period).max() - low.rolling(window=period).min()) / denom).where(denom > 0, 0.0)
    path = close.diff().abs().rolling(window=period - 1).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0)
    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    adx = pd.Series(comps['adx'], index=htf.index)
    warmup = net.isna() | atr.isna()
    big_move = return_eff.abs() >= return_eff_threshold
    up = return_eff > 0
    high_adx = adx >= adx_threshold
    wide = range_eff >= range_eff_threshold
    clean = (efficiency >= efficiency_threshold) & high_adx
    return np.select([warmup, big_move & up & clean, big_move & up, big_move & clean, big_move, high_adx, wide], [_WARMUP, _TREND_UP_CLEAN, _TREND_UP_CHOPPY, _TREND_DOWN_CLEAN, _TREND_DOWN_CHOPPY, _RANGING_DIRECTIONAL, _RANGING_VOLATILE], default=_RANGING_QUIET)

def _confirm_labels(raw: np.ndarray, confirm_buckets: int) -> np.ndarray:
    n = len(raw)
    confirmed = np.zeros(n, dtype=int)
    if confirm_buckets <= 1:
        return raw.copy()
    current = _WARMUP
    streak_label = _WARMUP
    streak = 0
    for i in range(n):
        lab = raw[i]
        if lab == _WARMUP:
            streak_label = _WARMUP
            streak = 0
        elif lab == streak_label:
            streak += 1
        else:
            streak_label = lab
            streak = 1
        if streak_label != _WARMUP and streak >= confirm_buckets and (streak_label != current):
            current = streak_label
        confirmed[i] = current
    return confirmed

def regime_adaptive_htf_core(df: pd.DataFrame, htf_factor: int=6, period: int=14, adx_threshold: float=20.0, return_eff_threshold: float=0.05, range_eff_threshold: float=0.03, efficiency_threshold: float=0.5, confirm_buckets: int=2, trend_entry: str='off', trend_drift_confirm: float=0.1, transition_window: int=6, pullback_z: float=1.0, fade_labels: str='ranging', breakout_lookback: int=10, mr_lookback: int=20, mr_entry_z: float=2.0, mr_exit_z: float=0.0, slow_trend_lookback: int=100, slow_veto_threshold: float=0.05, allow_short: bool=False) -> pd.DataFrame:
    htf_factor = max(int(htf_factor), 1)
    result = df.copy()
    n = len(result)
    result['signal'] = 0
    result['position'] = 0
    result['rah_label'] = ''
    result['rah_raw_label'] = ''
    result['rah_z'] = np.nan
    result['rah_slow_eff'] = np.nan
    if n == 0:
        return result
    close = result['close']
    high = result['high']
    low = result['low']
    htf, visible_at = _resample_htf(result, htf_factor)
    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    if len(htf) <= adx_period:
        return result
    raw_labels = _classify_buckets(htf, period, adx_threshold, return_eff_threshold, range_eff_threshold, efficiency_threshold)
    conf_labels = _confirm_labels(raw_labels, confirm_buckets)
    conf_native = _project_to_native(pd.Series(conf_labels, index=htf.index), visible_at, result.index).fillna(0).astype(int).to_numpy()
    raw_native = _project_to_native(pd.Series(raw_labels, index=htf.index), visible_at, result.index).fillna(0).astype(int).to_numpy()
    breakout_up = (close > high.rolling(window=breakout_lookback).max().shift(1)).values
    breakout_down = (close < low.rolling(window=breakout_lookback).min().shift(1)).values
    z = (close - close.rolling(window=mr_lookback).mean()) / close.rolling(window=mr_lookback).std()
    mr_long_trig = ((z > -mr_entry_z) & (z.shift(1) <= -mr_entry_z)).values
    mr_short_trig = ((z < mr_entry_z) & (z.shift(1) >= mr_entry_z)).values
    pb_long_trig = ((z > -pullback_z) & (z.shift(1) <= -pullback_z)).values
    pb_short_trig = ((z < pullback_z) & (z.shift(1) >= pullback_z)).values
    z_vals = z.values
    fade_family = _MR_FAMILY if fade_labels == 'all_mr' else (_RANGING_QUIET, _RANGING_VOLATILE)
    if slow_trend_lookback > 0:
        atr_native = atr_sma_series(high, low, close, mr_lookback)
        slow_denom = atr_native * slow_trend_lookback
        slow_eff = ((close - close.shift(slow_trend_lookback)) / slow_denom).where(slow_denom > 0, 0.0)
        veto_long_fade = (slow_eff <= -slow_veto_threshold).values
        veto_short_fade = (slow_eff >= slow_veto_threshold).values
        drift_ok_long = (slow_eff >= trend_drift_confirm).values
        drift_ok_short = (slow_eff <= -trend_drift_confirm).values
    else:
        slow_eff = pd.Series(np.nan, index=result.index)
        veto_long_fade = np.zeros(n, dtype=bool)
        veto_short_fade = np.zeros(n, dtype=bool)
        drift_ok_long = np.ones(n, dtype=bool)
        drift_ok_short = np.ones(n, dtype=bool)
    pos = 0
    positions = np.zeros(n, dtype=int)
    last_flip_i = -1
    prev_lab = _WARMUP
    for i in range(n):
        lab = conf_native[i]
        if lab != prev_lab and lab != _WARMUP:
            last_flip_i = i
        prev_lab = lab
        if lab == _WARMUP:
            pos = 0
            continue
        if pos == 1 and lab not in _UP_FAMILY:
            pos = 0
        elif pos == -1 and lab not in _DOWN_FAMILY:
            pos = 0
        elif pos == 2 and (not np.isnan(z_vals[i]) and z_vals[i] >= mr_exit_z or lab == _TREND_DOWN_CLEAN):
            pos = 0
        elif pos == -2 and (not np.isnan(z_vals[i]) and z_vals[i] <= -mr_exit_z or lab == _TREND_UP_CLEAN):
            pos = 0
        if pos == 0:
            if trend_entry == 'breakout':
                trend_long_trig = breakout_up[i]
                trend_short_trig = breakout_down[i]
            elif trend_entry == 'pullback':
                trend_long_trig = pb_long_trig[i]
                trend_short_trig = pb_short_trig[i]
            elif trend_entry == 'transition':
                trend_long_trig = i - last_flip_i < transition_window
                trend_short_trig = trend_long_trig
            else:
                trend_long_trig = False
                trend_short_trig = False
            if lab == _TREND_UP_CLEAN and trend_long_trig and drift_ok_long[i]:
                pos = 1
            elif allow_short and lab == _TREND_DOWN_CLEAN and trend_short_trig and drift_ok_short[i]:
                pos = -1
            elif lab in fade_family:
                if mr_long_trig[i] and (not veto_long_fade[i]):
                    pos = 2
                elif allow_short and mr_short_trig[i] and (not veto_short_fade[i]):
                    pos = -2
        positions[i] = 1 if pos > 0 else -1 if pos < 0 else 0
    pos_series = pd.Series(positions, index=result.index)
    result['position'] = pos_series
    result['signal'] = pos_series.diff().fillna(0).clip(-1, 1).astype(int)
    result['rah_label'] = [_LABEL_NAMES[int(code)] for code in conf_native]
    result['rah_raw_label'] = [_LABEL_NAMES[int(code)] for code in raw_native]
    result['rah_z'] = z
    result['rah_slow_eff'] = slow_eff
    return result
