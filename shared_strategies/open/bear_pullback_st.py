"""
Bear Pullback Short — short bear-market rallies into resistance.

Rules
-----
1. Bearish regime: EMA(mid) < EMA(long).
2. Trend strength: ADX > threshold.
3. Pullback into resistance: a recent bar's high touched EMA(short) or EMA(mid).
4. RSI rebounded into [rsi_lower, rsi_upper] during the pullback window.
5. Trigger: current close has lost EMA(short) (cross-down) OR closed below the
   prior bar's low — and the current close confirms by sitting below EMA(short).

Emits ``signal = -1`` on the trigger bar; otherwise 0.
"""

import numpy as np
import pandas as pd

from adx_trend import _compute_adx_components


def bear_pullback_st_core(
    df: pd.DataFrame,
    ema_short: int = 20,
    ema_mid: int = 50,
    ema_long: int = 200,
    adx_period: int = 14,
    adx_threshold: float = 20.0,
    rsi_period: int = 14,
    rsi_lower: float = 55.0,
    rsi_upper: float = 65.0,
    pullback_window: int = 5,
    pullback_touch_buffer_pct: float = 0.001,
) -> pd.DataFrame:
    """Generate short signals on failed rallies inside a bearish trend.

    Parameters
    ----------
    df : DataFrame with open, high, low, close columns
    ema_short / ema_mid / ema_long : EMAs for the pullback magnet and regime filter
    adx_period / adx_threshold : trend-strength gate (ADX > threshold)
    rsi_period : Wilder RSI lookback
    rsi_lower / rsi_upper : RSI band the rally must rebound into during the
        pullback window (default 55–65 — overbought relative to a bear trend)
    pullback_window : bars to look back for the rally touch + RSI rebound
    pullback_touch_buffer_pct : fraction by which the bar's high must *exceed*
        EMA(short)/EMA(mid) to count as a rally touch — separates real rallies
        into resistance from noisy wicks tagging the EMA

    Returns
    -------
    DataFrame with added columns:
        signal      : -1 (short), 0 (no entry)
        ema_short   : EMA(close, ema_short)
        ema_mid     : EMA(close, ema_mid)
        ema_long    : EMA(close, ema_long)
        adx         : Wilder ADX (0 during warmup)
        rsi         : Wilder RSI (NaN during warmup)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(ema_long, adx_period * 2, rsi_period) + pullback_window + 2
    if n < min_len:
        result["ema_short"] = result["close"].ewm(span=ema_short, adjust=False).mean()
        result["ema_mid"] = result["close"].ewm(span=ema_mid, adjust=False).mean()
        result["ema_long"] = result["close"].ewm(span=ema_long, adjust=False).mean()
        result["adx"] = 0.0
        result["rsi"] = np.nan
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    result["ema_short"] = close.ewm(span=ema_short, adjust=False).mean()
    result["ema_mid"] = close.ewm(span=ema_mid, adjust=False).mean()
    result["ema_long"] = close.ewm(span=ema_long, adjust=False).mean()

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    result["adx"] = comps["adx"]

    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))

    bearish_regime = result["ema_mid"] < result["ema_long"]
    strong_trend = result["adx"] > adx_threshold

    # Rally into resistance: high must *exceed* EMA(short) or EMA(mid) by a
    # buffer — guards against noisy wicks merely tagging the EMA in a downtrend.
    touch_mult = 1.0 + pullback_touch_buffer_pct
    pullback_touch = (high > result["ema_short"] * touch_mult) | (
        high > result["ema_mid"] * touch_mult
    )
    # .shift(1) so the rally must have happened on a *prior* bar — we never let
    # the trigger bar itself satisfy the touch/RSI condition.
    pullback_recent = (
        pullback_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )

    rsi_in_zone = (result["rsi"] >= rsi_lower) & (result["rsi"] <= rsi_upper)
    rsi_recent = (
        rsi_in_zone.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )

    prev_low = low.shift(1)
    prev_ema_short = result["ema_short"].shift(1)
    prev_close = close.shift(1)
    trigger_lose_ema = (close < result["ema_short"]) & (prev_close >= prev_ema_short)
    trigger_lower_low = close < prev_low
    trigger = trigger_lose_ema | trigger_lower_low

    confirm = close < result["ema_short"]

    short_mask = (
        bearish_regime
        & strong_trend
        & pullback_recent
        & rsi_recent
        & trigger
        & confirm
    )
    result.loc[short_mask, "signal"] = -1
    return result
