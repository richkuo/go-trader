
import numpy as np
import pandas as pd

from indicators_core import wilder_rsi

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

    result["rsi"] = wilder_rsi(close, rsi_period)

    bearish_regime = result["ema_mid"] < result["ema_long"]
    strong_trend = result["adx"] > adx_threshold

    touch_mult = 1.0 + pullback_touch_buffer_pct
    pullback_touch = (high > result["ema_short"] * touch_mult) | (
        high > result["ema_mid"] * touch_mult
    )
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
