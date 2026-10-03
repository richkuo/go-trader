
from __future__ import annotations

import numpy as np
import pandas as pd

from indicators_core import atr_sma


def anchored_vwap_reversion_core(
    df: pd.DataFrame,
    pivot_strength: int = 5,
    entry_atr_mult: float = 1.5,
    buffer_atr_mult: float = 0.25,
    confirm_bars: int = 2,
    atr_period: int = 14,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["avwap"] = np.nan
    result["anchor_index"] = -1
    result["atr"] = atr_sma(result, atr_period)
    if n < 2 * pivot_strength + 1 + confirm_bars:
        return result

    high = result["high"].astype(float).to_numpy()
    low = result["low"].astype(float).to_numpy()

    k = int(pivot_strength)
    is_pivot = np.zeros(n, dtype=bool)
    for i in range(k, n - k):
        wh = high[i - k:i + k + 1]
        wl = low[i - k:i + k + 1]
        wmax = wh.max()
        wmin = wl.min()
        is_high = high[i] == wmax and int((wh == wmax).sum()) == 1
        is_low = low[i] == wmin and int((wl == wmin).sum()) == 1
        if is_high or is_low:
            is_pivot[i] = True

    anchor = np.full(n, -1, dtype=int)
    last = -1
    for b in range(n):
        p = b - k
        if p >= 0 and is_pivot[p]:
            last = p
        anchor[b] = last
    result["anchor_index"] = anchor

    tp = ((result["high"] + result["low"] + result["close"]) / 3.0).to_numpy()
    vol = result["volume"].astype(float).to_numpy()
    pref_tpvol = np.concatenate([[0.0], np.cumsum(tp * vol)])
    pref_vol = np.concatenate([[0.0], np.cumsum(vol)])
    avwap = np.full(n, np.nan)
    for b in range(n):
        a = anchor[b]
        if a < 0:
            continue
        num = pref_tpvol[b + 1] - pref_tpvol[a]
        den = pref_vol[b + 1] - pref_vol[a]
        avwap[b] = tp[b] if den <= 0 else num / den
    result["avwap"] = avwap

    close = result["close"].astype(float).to_numpy()
    atr_arr = result["atr"].to_numpy()
    lower_band = avwap - entry_atr_mult * atr_arr
    upper_band = avwap + entry_atr_mult * atr_arr

    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1
        if b - 1 < 0 or anchor[b] < 0 or anchor[b - 1] < 0:
            continue
        if np.isnan(atr_arr[b]):
            continue
        buf = buffer_atr_mult * atr_arr[b]
        win_c = close[b:nbar + 1]
        if (low[b] <= lower_band[b]
                and close[b] >= lower_band[b] + buf
                and np.all(win_c >= lower_band[b:nbar + 1])
                and np.all(win_c < avwap[b:nbar + 1])
                and low[b - 1] > lower_band[b - 1]):
            sig[nbar] = 1
            continue
        if (high[b] >= upper_band[b]
                and close[b] <= upper_band[b] - buf
                and np.all(win_c <= upper_band[b:nbar + 1])
                and np.all(win_c > avwap[b:nbar + 1])
                and high[b - 1] < upper_band[b - 1]):
            sig[nbar] = -1
    result["signal"] = sig

    return result
