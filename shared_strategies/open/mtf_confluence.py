import numpy as np
import pandas as pd
_HTF_AGG = {'open': 'first', 'high': 'max', 'low': 'min', 'close': 'last'}

def _resample_htf(df: pd.DataFrame, htf_factor: int):
    native_td = None
    if isinstance(df.index, pd.DatetimeIndex) and len(df) >= 3:
        diffs = df.index.to_series().diff().dropna()
        if len(diffs) > 0:
            cadence = diffs.mode()
            if len(cadence) > 0 and cadence.iloc[0] > pd.Timedelta(0):
                native_td = cadence.iloc[0]
    if native_td is not None:
        htf_td = native_td * htf_factor
        htf = df[['open', 'high', 'low', 'close']].resample(htf_td, label='left', closed='left', origin='epoch').agg(_HTF_AGG).dropna(subset=['close'])
        visible_at = htf.index + htf_td - native_td
        return (htf, visible_at)
    n = len(df)
    n_full = n // htf_factor
    if n_full == 0:
        empty = pd.DataFrame(columns=list(_HTF_AGG))
        return (empty, df.index[:0])
    o = df['open'].to_numpy(dtype=float)[:n_full * htf_factor].reshape(n_full, htf_factor)
    h = df['high'].to_numpy(dtype=float)[:n_full * htf_factor].reshape(n_full, htf_factor)
    l = df['low'].to_numpy(dtype=float)[:n_full * htf_factor].reshape(n_full, htf_factor)
    c = df['close'].to_numpy(dtype=float)[:n_full * htf_factor].reshape(n_full, htf_factor)
    last_pos = np.arange(1, n_full + 1) * htf_factor - 1
    visible_at = df.index[last_pos]
    htf = pd.DataFrame({'open': o[:, 0], 'high': h.max(axis=1), 'low': l.min(axis=1), 'close': c[:, -1]}, index=visible_at)
    return (htf, visible_at)

def _project_to_native(values: pd.Series, visible_at, native_index) -> pd.Series:
    proj = pd.Series(values.to_numpy(), index=visible_at)
    return proj.reindex(native_index, method='ffill')

def mtf_confluence_core(df: pd.DataFrame, htf_factor: int=4, htf_ema_fast: int=20, htf_ema_slow: int=40, htf_sep_pct: float=0.001, ltf_ema: int=20, pullback_window: int=6, pullback_touch_buffer_pct: float=0.0, allow_short: bool=False) -> pd.DataFrame:
    htf_factor = max(int(htf_factor), 1)
    result = df.copy()
    result['signal'] = 0
    result['position'] = 0
    result['htf_trend'] = 0
    result['htf_ema_fast'] = np.nan
    result['htf_ema_slow'] = np.nan
    result['ltf_ema'] = np.nan
    n = len(result)
    if n < htf_factor + 2:
        return result
    close = result['close']
    high = result['high']
    low = result['low']
    htf, visible_at = _resample_htf(result, htf_factor)
    if len(htf) == 0:
        return result
    htf_close = htf['close']
    ema_fast_htf = htf_close.ewm(span=htf_ema_fast, adjust=False).mean()
    ema_slow_htf = htf_close.ewm(span=htf_ema_slow, adjust=False).mean()
    warm = np.arange(len(htf)) >= htf_ema_slow
    rising = ema_fast_htf > ema_fast_htf.shift(1)
    falling = ema_fast_htf < ema_fast_htf.shift(1)
    up_htf = (ema_fast_htf > ema_slow_htf * (1.0 + htf_sep_pct)) & rising & warm
    down_htf = (ema_fast_htf < ema_slow_htf * (1.0 - htf_sep_pct)) & falling & warm
    hold_up_htf = (ema_fast_htf > ema_slow_htf) & warm
    hold_down_htf = (ema_fast_htf < ema_slow_htf) & warm
    trend_htf = pd.Series(np.where(up_htf, 1, np.where(down_htf, -1, 0)), index=htf.index)
    trend = _project_to_native(trend_htf, visible_at, result.index).fillna(0).astype(int)
    hold_up = _project_to_native(hold_up_htf, visible_at, result.index).fillna(False).astype(bool).to_numpy()
    hold_down = _project_to_native(hold_down_htf, visible_at, result.index).fillna(False).astype(bool).to_numpy()
    result['htf_trend'] = trend
    result['htf_ema_fast'] = _project_to_native(ema_fast_htf, visible_at, result.index)
    result['htf_ema_slow'] = _project_to_native(ema_slow_htf, visible_at, result.index)
    ltf_ema_s = close.ewm(span=ltf_ema, adjust=False).mean()
    result['ltf_ema'] = ltf_ema_s
    long_touch = low < ltf_ema_s * (1.0 - pullback_touch_buffer_pct)
    long_pullback = long_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    short_touch = high > ltf_ema_s * (1.0 + pullback_touch_buffer_pct)
    short_pullback = short_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    prev_high = high.shift(1)
    prev_low = low.shift(1)
    long_trig = ((close > ltf_ema_s) & (close > prev_high) & long_pullback).to_numpy()
    short_trig = ((close < ltf_ema_s) & (close < prev_low) & short_pullback).to_numpy()
    trend_arr = trend.to_numpy()
    pos = np.zeros(n, dtype=int)
    state = 0
    for i in range(n):
        if state == 1 and (not hold_up[i]):
            state = 0
        elif state == -1 and (not hold_down[i]):
            state = 0
        if state == 0:
            if long_trig[i] and trend_arr[i] == 1:
                state = 1
            elif allow_short and short_trig[i] and (trend_arr[i] == -1):
                state = -1
        pos[i] = state
    result['position'] = pos
    result['signal'] = np.clip(np.diff(pos, prepend=0), -1, 1)
    return result
