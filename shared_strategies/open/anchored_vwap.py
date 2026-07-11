"""Anchored VWAP (AVWAP) — single VWAP anchored to the most recent *confirmed*
swing pivot, traded as a buffered support/resistance flip.

Design: docs/superpowers/specs/2026-06-15-anchored-vwap-strategy-design.md

Unlike the session-reset VWAP strategies (vwap_reversion, vwap_rejection_st),
this accumulates the volume-weighted price from a structural event (a confirmed
swing pivot) forward and re-anchors only when a newer pivot confirms.

ATR/RSI come from the shared open-tree module ``indicators_core`` (#1281) —
importable at module load without shared_tools on sys.path (the registry parity
test loads registry.py via importlib with a bare sys.path; indicators_core lives
in this directory, which the registry inserts before importing core modules).
"""

from __future__ import annotations

import numpy as np
import pandas as pd

from indicators_core import atr_sma, wilder_rsi


def anchored_vwap_core(
    df: pd.DataFrame,
    pivot_strength: int = 5,
    buffer_atr_mult: float = 0.25,
    confirm_bars: int = 2,
    atr_period: int = 14,
    gate_rsi_period: int = 0,
    gate_rsi_level: float = 50.0,
    gate_ema_period: int = 0,
) -> pd.DataFrame:
    """Single-AVWAP support/resistance-flip signals.

    Parameters
    ----------
    df : OHLCV DataFrame (open, high, low, close, volume).
    pivot_strength : bars required on EACH side of a swing pivot to confirm it
        (strict max high / strict min low). A pivot at bar p is only knowable at
        bar p + pivot_strength — the look-ahead guarantee.
    buffer_atr_mult : the buffered breach must clear the AVWAP by
        buffer_atr_mult * ATR on the breach bar.
    confirm_bars : bars the close must hold on the correct side of the AVWAP
        (inclusive of the breach bar) before a signal fires.
    atr_period : lookback for the inline ATR.
    gate_rsi_period : momentum gate (#1017, default-off): when > 0, a long
        only fires with RSI(gate_rsi_period) >= gate_rsi_level on the signal
        bar and a short only with RSI <= gate_rsi_level (equality passes both
        ways). NaN warmup bars pass — the same fail-open semantics as the
        #982 ``htf_gate_mode="veto"`` neutral state. 0 disables the gate and
        the output is bit-identical to the pre-gate strategy.
    gate_rsi_level : RSI midline the gate compares against.
    gate_ema_period : trend gate (#1017, default-off): when > 0, a long only
        fires with the signal-bar close >= EMA(gate_ema_period) and a short
        only with close <= it (equality passes). Bars before the EMA has
        accrued ``gate_ema_period`` inputs are warmup and pass (mirrors the
        #982 EMA-warmup convention). 0 disables the gate.

    Returns
    -------
    DataFrame with added columns:
        signal       : +1 (long reclaim), -1 (short breakdown), 0 otherwise
        avwap        : anchored VWAP (NaN before the first confirmed anchor)
        anchor_index : bar index of the active anchor (-1 before the first one)
        atr          : inline ATR
        gate_rsi     : gate RSI series (only when gate_rsi_period > 0)
        gate_ema     : gate EMA series (only when gate_ema_period > 0)
    """
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

    # --- trigger: buffered S/R flip, fire-once via fresh-crossing clause ---
    close = result["close"].astype(float).to_numpy()
    atr_arr = result["atr"].to_numpy()
    cb = int(confirm_bars)
    sig = np.zeros(n, dtype=int)
    for nbar in range(n):
        b = nbar - cb + 1                       # window start
        if b - 1 < 0 or anchor[b] < 0:
            continue
        if np.isnan(avwap[b - 1]) or np.isnan(atr_arr[b]):
            continue
        buf = buffer_atr_mult * atr_arr[b]
        win_c = close[b:nbar + 1]
        win_v = avwap[b:nbar + 1]
        # LONG: held above, buffered breach on window-start, prior bar below.
        if (np.all(win_c >= win_v)
                and close[b] >= avwap[b] + buf
                and close[b - 1] < avwap[b - 1]):
            sig[nbar] = 1
            continue
        # SHORT: mirror.
        if (np.all(win_c <= win_v)
                and close[b] <= avwap[b] - buf
                and close[b - 1] > avwap[b - 1]):
            sig[nbar] = -1

    # --- momentum/trend gate (#1017 rider B, default-off) ---
    # Applied after the flip trigger so the gates see exactly the signals the
    # strategy would otherwise emit (same layering as the #982 chart_pattern
    # HTF gate). Warmup bars fail open: a gate with no data never blocks.
    if gate_rsi_period and int(gate_rsi_period) > 0:
        rsi = wilder_rsi(result["close"].astype(float), int(gate_rsi_period))
        result["gate_rsi"] = rsi
        r = rsi.to_numpy()
        level = float(gate_rsi_level)
        blocked = ((sig == 1) & (r < level)) | ((sig == -1) & (r > level))
        blocked &= ~np.isnan(r)
        sig[blocked] = 0
    if gate_ema_period and int(gate_ema_period) > 0:
        p = int(gate_ema_period)
        ema = result["close"].astype(float).ewm(span=p, adjust=False).mean()
        result["gate_ema"] = ema
        e = ema.to_numpy()
        warm = np.arange(n) >= p
        blocked = warm & (((sig == 1) & (close < e)) | ((sig == -1) & (close > e)))
        sig[blocked] = 0
    result["signal"] = sig

    return result
