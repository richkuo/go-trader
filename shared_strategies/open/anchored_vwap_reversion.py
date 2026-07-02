"""Anchored VWAP Reversion (AVWAP reversion) — mean-reversion TO the single
pivot-anchored VWAP line: long a buffered snap-back from an ATR-measured
stretch below the line, short the mirror stretch above it.

Design: docs/superpowers/specs/2026-07-02-anchored-vwap-reversion-strategy-design.md

Complementary to anchored_vwap (#1016), which trades a buffered breach
*through* the same line — here every window bar must close strictly on the
stretched side of the line (a reclaim kills the fire; that regime belongs to
the flip strategy) — and to anchored_vwap_channel (#1169), which fades touches
of its two anchored lines from inside the channel rather than an ATR-measured
stretch beyond the one type-agnostic line.

ATR is inlined (rolling-mean True Range, integer-round only when atr >= 100) to
match standard_atr WITHOUT importing shared_tools — open strategies cannot
assume shared_tools is on sys.path at module-load time (the registry parity test
loads registry.py via importlib without it, so a top-level import would raise
ModuleNotFoundError). The inline copy is byte-identical to standard_atr.
"""

from __future__ import annotations

import numpy as np
import pandas as pd


def _inline_atr(df: pd.DataFrame, period: int) -> pd.Series:
    """ATR via simple rolling mean of True Range (standard_atr convention)."""
    high = df["high"].astype(float)
    low = df["low"].astype(float)
    prev_close = df["close"].astype(float).shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    atr = tr.rolling(window=period).mean()
    return atr.where(atr < 100, atr.round(0))


def anchored_vwap_reversion_core(
    df: pd.DataFrame,
    pivot_strength: int = 5,
    entry_atr_mult: float = 1.5,
    buffer_atr_mult: float = 0.25,
    confirm_bars: int = 2,
    atr_period: int = 14,
) -> pd.DataFrame:
    """Single-AVWAP stretch-and-snap-back reversion signals.

    Parameters
    ----------
    df : OHLCV DataFrame (open, high, low, close, volume).
    pivot_strength : bars required on EACH side of a swing pivot to confirm it
        (strict max high / strict min low). A pivot at bar p is only knowable at
        bar p + pivot_strength — the look-ahead guarantee.
    entry_atr_mult : band distance — the trigger bar's extreme must reach
        entry_atr_mult * ATR beyond the AVWAP before a fade is considered.
    buffer_atr_mult : the trigger bar's close must recover back inside the band
        by buffer_atr_mult * ATR (the snap-back confirmation).
    confirm_bars : bars the close must hold inside the stretch zone — between
        the band and the line — (inclusive of the trigger bar) before a signal
        fires.
    atr_period : lookback for the inline ATR.

    Returns
    -------
    DataFrame with added columns:
        signal       : +1 (downside stretch fading long), -1 (upside stretch
                       fading short), 0 otherwise
        avwap        : anchored VWAP (NaN before the first confirmed anchor)
        anchor_index : bar index of the active anchor (-1 before the first one)
        atr          : inline ATR
    """
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["avwap"] = np.nan
    result["anchor_index"] = -1
    result["atr"] = _inline_atr(result, atr_period)
    if n < 2 * pivot_strength + 1 + confirm_bars:
        return result

    high = result["high"].astype(float).to_numpy()
    low = result["low"].astype(float).to_numpy()

    # --- strict swing pivots (unique max high / unique min low in window) ---
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

    # --- anchor in effect at each bar: most recent pivot confirmed by then.
    # A pivot at p becomes knowable at bar p + k.
    anchor = np.full(n, -1, dtype=int)
    last = -1
    for b in range(n):
        p = b - k
        if p >= 0 and is_pivot[p]:
            last = p
        anchor[b] = last
    result["anchor_index"] = anchor

    # --- AVWAP via global prefix sums (exact across re-anchors) ---
    tp = ((result["high"] + result["low"] + result["close"]) / 3.0).to_numpy()
    vol = result["volume"].astype(float).to_numpy()
    pref_tpvol = np.concatenate([[0.0], np.cumsum(tp * vol)])  # pref[i] = sum first i
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

    # --- per-bar ATR bands around the live line ---
    close = result["close"].astype(float).to_numpy()
    atr_arr = result["atr"].to_numpy()
    lower_band = avwap - entry_atr_mult * atr_arr
    upper_band = avwap + entry_atr_mult * atr_arr

    # --- trigger: stretch touch + buffered snap-back + zone hold, fire-once
    # via fresh-touch clause. NaN bands (ATR warmup / no anchor) make every
    # comparison false — fail quiet.
    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1                       # window start (trigger bar)
        if b - 1 < 0 or anchor[b] < 0 or anchor[b - 1] < 0:
            continue                            # freshness reference needs a band too
        if np.isnan(atr_arr[b]):
            continue
        buf = buffer_atr_mult * atr_arr[b]
        win_c = close[b:nbar + 1]
        # LONG: stretch touch below the lower band, buffered snap-back, every
        # window close held between the band and the line (target not reached).
        if (low[b] <= lower_band[b]
                and close[b] >= lower_band[b] + buf
                and np.all(win_c >= lower_band[b:nbar + 1])
                and np.all(win_c < avwap[b:nbar + 1])
                and low[b - 1] > lower_band[b - 1]):
            sig[nbar] = 1
            continue
        # SHORT: mirror on the upper band.
        if (high[b] >= upper_band[b]
                and close[b] <= upper_band[b] - buf
                and np.all(win_c <= upper_band[b:nbar + 1])
                and np.all(win_c > avwap[b:nbar + 1])
                and high[b - 1] < upper_band[b - 1]):
            sig[nbar] = -1
    result["signal"] = sig

    return result
