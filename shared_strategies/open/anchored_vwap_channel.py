"""Anchored VWAP Channel (AVWAP channel) — dual VWAPs anchored to the most
recent *confirmed* swing low (support line) and swing high (resistance line),
traded as range-edge mean reversion: long a buffered bounce off the lower
line, short a buffered rejection off the upper line.

Design: docs/superpowers/specs/2026-07-01-anchored-vwap-channel-strategy-design.md

Complementary to anchored_vwap (#1016), which trades a buffered breach
*through* its single type-agnostic line: here a close beyond either line fails
the hold clause by construction — the channel strategy only fades touches that
hold inside the channel. No trading while the lines are inverted
(support >= resistance) or the channel is thinner than
min_width_atr_mult * ATR on the trigger bar.

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


def anchored_vwap_channel_core(
    df: pd.DataFrame,
    pivot_strength: int = 5,
    buffer_atr_mult: float = 0.25,
    confirm_bars: int = 2,
    min_width_atr_mult: float = 1.5,
    atr_period: int = 14,
) -> pd.DataFrame:
    """Dual-AVWAP channel bounce/rejection signals.

    Parameters
    ----------
    df : OHLCV DataFrame (open, high, low, close, volume).
    pivot_strength : bars required on EACH side of a swing pivot to confirm it
        (strict max high / strict min low). A pivot at bar p is only knowable at
        bar p + pivot_strength — the look-ahead guarantee.
    buffer_atr_mult : the trigger bar's close must recover past its line by
        buffer_atr_mult * ATR (reclaim above support / rejection below
        resistance).
    confirm_bars : bars the close must hold on the channel side of the touched
        line (inclusive of the touch bar) before a signal fires.
    min_width_atr_mult : minimum channel width, in ATR multiples on the trigger
        bar; a thinner (or inverted) channel emits no signal.
    atr_period : lookback for the inline ATR.

    Returns
    -------
    DataFrame with added columns:
        signal           : +1 (support bounce), -1 (resistance rejection), 0 otherwise
        avwap_support    : AVWAP anchored at the last confirmed swing LOW (NaN before it)
        avwap_resistance : AVWAP anchored at the last confirmed swing HIGH (NaN before it)
        anchor_low_index : bar index of the active swing-low anchor (-1 before the first one)
        anchor_high_index: bar index of the active swing-high anchor (-1 before the first one)
        atr              : inline ATR
    """
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["avwap_support"] = np.nan
    result["avwap_resistance"] = np.nan
    result["anchor_low_index"] = -1
    result["anchor_high_index"] = -1
    result["atr"] = _inline_atr(result, atr_period)
    if n < 2 * pivot_strength + 1 + confirm_bars:
        return result

    high = result["high"].astype(float).to_numpy()
    low = result["low"].astype(float).to_numpy()

    # --- strict swing pivots, tracked by type (unique max high / unique min low) ---
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

    # --- per-type anchor in effect at each bar: most recent pivot of that type
    # confirmed by then. A pivot at p becomes knowable at bar p + k.
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
    result["anchor_high_index"] = anchor_high
    result["anchor_low_index"] = anchor_low

    # --- both AVWAPs via global prefix sums (exact across re-anchors) ---
    tp = ((result["high"] + result["low"] + result["close"]) / 3.0).to_numpy()
    vol = result["volume"].astype(float).to_numpy()
    pref_tpvol = np.concatenate([[0.0], np.cumsum(tp * vol)])  # pref[i] = sum first i
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
    result["avwap_support"] = support
    result["avwap_resistance"] = resistance

    # --- trigger: touch + buffered reclaim + N-bar hold, fire-once via fresh
    # touch. Channel validity (both lines defined, not inverted, wide enough)
    # is evaluated on the trigger bar b.
    close = result["close"].astype(float).to_numpy()
    atr_arr = result["atr"].to_numpy()
    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1                       # window start (touch bar)
        if b - 1 < 0 or anchor_low[b] < 0 or anchor_high[b] < 0:
            continue
        if anchor_low[b - 1] < 0 or anchor_high[b - 1] < 0:
            continue                            # freshness reference needs lines too
        if np.isnan(atr_arr[b]):
            continue
        if support[b] >= resistance[b]:
            continue                            # inverted channel: fail quiet
        if resistance[b] - support[b] < min_width_atr_mult * atr_arr[b]:
            continue                            # channel too thin to fade
        buf = buffer_atr_mult * atr_arr[b]
        # LONG: fresh touch of support, buffered reclaim, held above the line.
        if (low[b] <= support[b]
                and close[b] >= support[b] + buf
                and np.all(close[b:nbar + 1] >= support[b:nbar + 1])
                and low[b - 1] > support[b - 1]):
            sig[nbar] = 1
            continue
        # SHORT: mirror on the resistance line.
        if (high[b] >= resistance[b]
                and close[b] <= resistance[b] - buf
                and np.all(close[b:nbar + 1] <= resistance[b:nbar + 1])
                and high[b - 1] < resistance[b - 1]):
            sig[nbar] = -1
    result["signal"] = sig

    return result
