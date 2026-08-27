from __future__ import annotations
import numpy as np
import pandas as pd
from indicators_core import atr_sma

def anchored_vwap_channel_core(df: pd.DataFrame, pivot_strength: int=5, buffer_atr_mult: float=0.25, confirm_bars: int=2, min_width_atr_mult: float=1.5, atr_period: int=14) -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result['signal'] = 0
    result['avwap_support'] = np.nan
    result['avwap_resistance'] = np.nan
    result['anchor_low_index'] = -1
    result['anchor_high_index'] = -1
    result['atr'] = atr_sma(result, atr_period)
    if n < 2 * pivot_strength + 1 + confirm_bars:
        return result
    high = result['high'].astype(float).to_numpy()
    low = result['low'].astype(float).to_numpy()
    k = int(pivot_strength)
    is_pivot_high = np.zeros(n, dtype=bool)
    is_pivot_low = np.zeros(n, dtype=bool)
    for i in range(k, n - k):
        wh = high[i - k:i + k + 1]
        wl = low[i - k:i + k + 1]
        wmax = wh.max()
        wmin = wl.min()
        if high[i] == wmax and int((wh == wmax).sum()) == 1:
            is_pivot_high[i] = True
        if low[i] == wmin and int((wl == wmin).sum()) == 1:
            is_pivot_low[i] = True
    anchor_high = np.full(n, -1, dtype=int)
    anchor_low = np.full(n, -1, dtype=int)
    last_high = -1
    last_low = -1
    for b in range(n):
        p = b - k
        if p >= 0:
            if is_pivot_high[p]:
                last_high = p
            if is_pivot_low[p]:
                last_low = p
        anchor_high[b] = last_high
        anchor_low[b] = last_low
    result['anchor_high_index'] = anchor_high
    result['anchor_low_index'] = anchor_low
    tp = ((result['high'] + result['low'] + result['close']) / 3.0).to_numpy()
    vol = result['volume'].astype(float).to_numpy()
    pref_tpvol = np.concatenate([[0.0], np.cumsum(tp * vol)])
    pref_vol = np.concatenate([[0.0], np.cumsum(vol)])

    def _avwap_from(anchor: np.ndarray) -> np.ndarray:
        line = np.full(n, np.nan)
        for b in range(n):
            a = anchor[b]
            if a < 0:
                continue
            num = pref_tpvol[b + 1] - pref_tpvol[a]
            den = pref_vol[b + 1] - pref_vol[a]
            line[b] = tp[b] if den <= 0 else num / den
        return line
    support = _avwap_from(anchor_low)
    resistance = _avwap_from(anchor_high)
    result['avwap_support'] = support
    result['avwap_resistance'] = resistance
    close = result['close'].astype(float).to_numpy()
    atr_arr = result['atr'].to_numpy()
    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1
        if b - 1 < 0 or anchor_low[b] < 0 or anchor_high[b] < 0:
            continue
        if anchor_low[b - 1] < 0 or anchor_high[b - 1] < 0:
            continue
        if np.isnan(atr_arr[b]):
            continue
        if support[b] >= resistance[b]:
            continue
        if resistance[b] - support[b] < min_width_atr_mult * atr_arr[b]:
            continue
        buf = buffer_atr_mult * atr_arr[b]
        if low[b] <= support[b] and close[b] >= support[b] + buf and np.all(close[b:nbar + 1] >= support[b:nbar + 1]) and (low[b - 1] > support[b - 1]):
            sig[nbar] = 1
            continue
        if high[b] >= resistance[b] and close[b] <= resistance[b] - buf and np.all(close[b:nbar + 1] <= resistance[b:nbar + 1]) and (high[b - 1] < resistance[b - 1]):
            sig[nbar] = -1
    result['signal'] = sig
    return result
