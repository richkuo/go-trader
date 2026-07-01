"""
Momentum Pro — trend-pullback momentum with multi-filter confirmation.

The naive ``momentum`` strategy buys any rate-of-change spike, which chases
breakouts and gets chopped up in ranges. Momentum Pro only enters *in the
direction of an established trend, after a pullback, on a confirmed resumption*
— the entry quality and selectivity are where the edge lives.

Rules (long; short is the mirror)
---------------------------------
1. Trend regime:  EMA(fast) > EMA(mid) > EMA(long)  (stacked bullish).
2. Trend strength: ADX > threshold (a real trend, not a single-bar spike).
3. Pullback:      a recent bar's low tagged EMA(fast) within ``pullback_window``
                  (price came back to the trend before resuming).
4. Resumption:    current close reclaims EMA(fast) AND breaks the prior bar's
                  high (momentum turning back up).
5. Volume:        entry-bar volume > ``vol_mult`` × SMA(volume) — skip when
                  ``vol_mult`` <= 0.

Emits ``signal = 1`` (long) / ``-1`` (short) on the trigger bar, else 0.

Optional volatility-targeted entry sizing (#980, default OFF): when
``vol_target_atr_pct > 0`` an ``entry_fraction`` column scales the notional
committed at open inversely with ATR/close. Signals are never affected.
"""

import numpy as np
import pandas as pd

from adx_trend import _compute_adx_components


def momentum_pro_core(
    df: pd.DataFrame,
    ema_fast: int = 20,
    ema_mid: int = 50,
    ema_long: int = 200,
    adx_period: int = 14,
    adx_threshold: float = 20.0,
    pullback_window: int = 6,
    pullback_touch_buffer_pct: float = 0.0,
    vol_period: int = 20,
    vol_mult: float = 1.2,
    vol_target_atr_pct: float = 0.0,
    vol_target_atr_period: int = 14,
    vol_target_min_fraction: float = 0.10,
) -> pd.DataFrame:
    """Generate trend-pullback momentum signals (bidirectional).

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    ema_fast / ema_mid / ema_long : EMAs for the stacked trend regime + pullback magnet
    adx_period / adx_threshold : trend-strength gate (ADX > threshold)
    pullback_window : bars to look back for the EMA(fast) tag
    pullback_touch_buffer_pct : fraction the bar must pierce EMA(fast) by to count
        as a pullback tag (0 = a plain touch qualifies)
    vol_period : SMA lookback for the volume baseline
    vol_mult : entry-bar volume must exceed vol_mult × SMA(volume); <= 0 disables
    vol_target_atr_pct : volatility-targeted entry sizing (#980, default OFF).
        When > 0, emit an ``entry_fraction`` column scaling the notional
        committed at open inversely with realized vol:
        ``clip(vol_target_atr_pct / (ATR / close), vol_target_min_fraction, 1.0)``
        — full size when ATR/close <= the target, proportionally smaller when
        the market is more volatile. Signals are NEVER changed by this knob;
        <= 0 (the default) emits no column and is byte-identical to today.
        Deliberately NOT a registered default param (``--list-json`` stays
        byte-identical); reach it via ``--params``.
    vol_target_atr_period : rolling True-Range mean lookback for the sizing ATR
    vol_target_min_fraction : floor on the emitted fraction (avoids dust entries)

    Returns
    -------
    DataFrame with added columns:
        signal     : 1 (long), -1 (short), 0 (no entry)
        ema_fast / ema_mid / ema_long : the regime EMAs
        adx        : Wilder ADX (0 during warmup)
        vol_sma    : SMA(volume, vol_period)
        entry_fraction : only when ``vol_target_atr_pct > 0`` — per-bar entry
            size fraction in (0, 1] (NaN during ATR warmup = full notional)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(ema_long, adx_period * 2, vol_period) + pullback_window + 2
    if n < min_len:
        result["ema_fast"] = result["close"].ewm(span=ema_fast, adjust=False).mean()
        result["ema_mid"] = result["close"].ewm(span=ema_mid, adjust=False).mean()
        result["ema_long"] = result["close"].ewm(span=ema_long, adjust=False).mean()
        result["adx"] = 0.0
        result["vol_sma"] = np.nan
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]
    volume = result["volume"]

    result["ema_fast"] = close.ewm(span=ema_fast, adjust=False).mean()
    result["ema_mid"] = close.ewm(span=ema_mid, adjust=False).mean()
    result["ema_long"] = close.ewm(span=ema_long, adjust=False).mean()

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    result["adx"] = comps["adx"]

    result["vol_sma"] = volume.rolling(window=vol_period).mean()

    ema_fast_s = result["ema_fast"]
    ema_mid_s = result["ema_mid"]
    ema_long_s = result["ema_long"]

    bull_regime = (ema_fast_s > ema_mid_s) & (ema_mid_s > ema_long_s)
    bear_regime = (ema_fast_s < ema_mid_s) & (ema_mid_s < ema_long_s)
    strong_trend = result["adx"] > adx_threshold

    if vol_mult > 0:
        vol_confirm = volume > (result["vol_sma"] * vol_mult)
    else:
        vol_confirm = pd.Series(True, index=result.index)

    # Positive buffer *tightens* the gate: the bar must pierce EMA(fast) by the
    # fraction (low below for longs, high above for shorts). buffer=0 → plain touch.
    # Long pullback: a recent prior bar's low pierced below EMA(fast).
    long_touch = low < (ema_fast_s * (1.0 - pullback_touch_buffer_pct))
    long_pullback = (
        long_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )
    # Short pullback: a recent prior bar's high pierced above EMA(fast).
    short_touch = high > (ema_fast_s * (1.0 + pullback_touch_buffer_pct))
    short_pullback = (
        short_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )

    prev_high = high.shift(1)
    prev_low = low.shift(1)

    long_trigger = (close > ema_fast_s) & (close > prev_high)
    short_trigger = (close < ema_fast_s) & (close < prev_low)

    long_mask = bull_regime & strong_trend & long_pullback & long_trigger & vol_confirm
    short_mask = bear_regime & strong_trend & short_pullback & short_trigger & vol_confirm

    result.loc[long_mask, "signal"] = 1
    result.loc[short_mask, "signal"] = -1

    # #980: volatility-targeted entry sizing (default OFF — no column, and no
    # effect on signals either way). ATR follows the standard_atr convention
    # (rolling-mean True Range, integer-round only when ATR >= 100 — keep in
    # sync with shared_tools/atr.py and the other inline copies).
    if vol_target_atr_pct > 0:
        prev_close = close.shift(1)
        tr = pd.concat(
            [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
            axis=1,
        ).max(axis=1)
        atr = tr.rolling(window=vol_target_atr_period).mean()
        atr = atr.where(atr < 100, atr.round(0))
        atr_pct = atr / close
        fraction = (vol_target_atr_pct / atr_pct).clip(
            lower=vol_target_min_fraction, upper=1.0,
        )
        # Warmup / degenerate ATR (NaN or <= 0) → NaN = "no opinion"; the
        # engine resolves NaN to full notional, today's behavior.
        result["entry_fraction"] = fraction.where(atr_pct > 0)

    return result
