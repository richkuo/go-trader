
import numpy as np
import pandas as pd

from indicators_core import wilder_rsi

from adx_trend import _compute_adx_components


def mean_reversion_pro_core(
    df: pd.DataFrame,
    lookback: int = 30,
    entry_std: float = 2.0,
    adx_period: int = 14,
    adx_max: float = 25.0,
    rsi_period: int = 14,
    rsi_oversold: float = 30.0,
    rsi_overbought: float = 70.0,
    confirm_window: int = 3,
    touch_entry: int = 0,
    turn_entry: int = 0,
) -> pd.DataFrame:
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(lookback, adx_period * 2, rsi_period) + confirm_window + 2
    if n < min_len:
        result["z_score"] = np.nan
        result["adx"] = 0.0
        result["rsi"] = np.nan
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    rolling_mean = close.rolling(window=lookback).mean()
    rolling_std = close.rolling(window=lookback).std()
    result["z_score"] = (close - rolling_mean) / rolling_std

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    result["adx"] = comps["adx"]

    result["rsi"] = wilder_rsi(close, rsi_period)

    z = result["z_score"]
    no_trend = result["adx"] < adx_max

    long_revert = (z > -entry_std) & (z.shift(1) <= -entry_std)
    short_revert = (z < entry_std) & (z.shift(1) >= entry_std)

    rsi_was_oversold = (
        (result["rsi"] < rsi_oversold)
        .shift(1)
        .rolling(window=confirm_window)
        .max()
        .fillna(0)
        .astype(bool)
    )
    rsi_was_overbought = (
        (result["rsi"] > rsi_overbought)
        .shift(1)
        .rolling(window=confirm_window)
        .max()
        .fillna(0)
        .astype(bool)
    )

    long_mask = no_trend & long_revert & rsi_was_oversold
    short_mask = no_trend & short_revert & rsi_was_overbought

    if touch_entry or turn_entry:
        rsi_evidence_long = (result["rsi"] < rsi_oversold) | rsi_was_oversold
        rsi_evidence_short = (result["rsi"] > rsi_overbought) | rsi_was_overbought
        if touch_entry:
            long_touch = (z <= -entry_std) & (z.shift(1) > -entry_std)
            short_touch = (z >= entry_std) & (z.shift(1) < entry_std)
            long_mask |= no_trend & long_touch & rsi_evidence_long
            short_mask |= no_trend & short_touch & rsi_evidence_short
        if turn_entry:
            long_turn = (z <= -entry_std) & (z > z.shift(1))
            short_turn = (z >= entry_std) & (z < z.shift(1))
            long_mask |= no_trend & long_turn & rsi_evidence_long
            short_mask |= no_trend & short_turn & rsi_evidence_short

    result.loc[long_mask, "signal"] = 1
    result.loc[short_mask, "signal"] = -1
    return result
