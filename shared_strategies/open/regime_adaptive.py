
import numpy as np
import pandas as pd

from indicators_core import atr_sma_series

from adx_trend import _compute_adx_components

_COMPOSITE_ADX_PERIOD_CAP = 14

_WARMUP = 0
_TREND_UP_CLEAN = 1
_TREND_UP_CHOPPY = 2
_TREND_DOWN_CLEAN = -1
_TREND_DOWN_CHOPPY = -2
_RANGING_DIRECTIONAL = 3
_RANGING_VOLATILE = 4
_RANGING_QUIET = 5

_LABEL_NAMES = {
    _WARMUP: "",
    _TREND_UP_CLEAN: "trending_up_clean",
    _TREND_UP_CHOPPY: "trending_up_choppy",
    _TREND_DOWN_CLEAN: "trending_down_clean",
    _TREND_DOWN_CHOPPY: "trending_down_choppy",
    _RANGING_DIRECTIONAL: "ranging_directional",
    _RANGING_VOLATILE: "ranging_volatile",
    _RANGING_QUIET: "ranging_quiet",
}


def regime_adaptive_core(
    df: pd.DataFrame,
    period: int = 20,
    adx_threshold: float = 20.0,
    return_eff_threshold: float = 0.05,
    range_eff_threshold: float = 0.03,
    efficiency_threshold: float = 0.5,
    breakout_lookback: int = 10,
    mr_lookback: int = 20,
    mr_entry_z: float = 2.0,
    mr_exit_z: float = 0.0,
    slow_trend_lookback: int = 100,
    slow_veto_threshold: float = 0.05,
    allow_short: bool = False,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)

    result["signal"] = 0
    result["position"] = 0
    result["ra_label"] = ""
    result["ra_return_eff"] = np.nan
    result["ra_range_eff"] = np.nan
    result["ra_efficiency"] = np.nan
    result["ra_adx"] = 0.0
    result["ra_z"] = np.nan
    result["ra_slow_eff"] = np.nan
    adx_period = min(period, _COMPOSITE_ADX_PERIOD_CAP)
    if n <= adx_period:
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    atr = atr_sma_series(high, low, close, period)

    denom = atr * period
    net = close - close.shift(period - 1)
    return_eff = (net / denom).where(denom > 0, 0.0)
    range_eff = (
        (high.rolling(window=period).max() - low.rolling(window=period).min()) / denom
    ).where(denom > 0, 0.0)
    path = close.diff().abs().rolling(window=period - 1).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0)

    comps = _compute_adx_components(high.values, low.values, close.values, adx_period)
    adx = pd.Series(comps["adx"], index=result.index)

    warmup = net.isna() | atr.isna()
    big_move = return_eff.abs() >= return_eff_threshold
    up = return_eff > 0
    high_adx = adx >= adx_threshold
    wide = range_eff >= range_eff_threshold
    clean = (efficiency >= efficiency_threshold) & high_adx
    labels = np.select(
        [
            warmup,
            big_move & up & clean,
            big_move & up,
            big_move & clean,
            big_move,
            high_adx,
            wide,
        ],
        [
            _WARMUP,
            _TREND_UP_CLEAN,
            _TREND_UP_CHOPPY,
            _TREND_DOWN_CLEAN,
            _TREND_DOWN_CHOPPY,
            _RANGING_DIRECTIONAL,
            _RANGING_VOLATILE,
        ],
        default=_RANGING_QUIET,
    )

    breakout_up = (close > high.rolling(window=breakout_lookback).max().shift(1)).values
    breakout_down = (close < low.rolling(window=breakout_lookback).min().shift(1)).values
    z = (close - close.rolling(window=mr_lookback).mean()) / close.rolling(window=mr_lookback).std()
    mr_long_trig = ((z > -mr_entry_z) & (z.shift(1) <= -mr_entry_z)).values
    mr_short_trig = ((z < mr_entry_z) & (z.shift(1) >= mr_entry_z)).values
    z_vals = z.values

    if slow_trend_lookback > 0:
        slow_denom = atr * slow_trend_lookback
        slow_eff = ((close - close.shift(slow_trend_lookback)) / slow_denom).where(
            slow_denom > 0, 0.0
        )
        veto_long_fade = (slow_eff <= -slow_veto_threshold).values
        veto_short_fade = (slow_eff >= slow_veto_threshold).values
    else:
        slow_eff = pd.Series(np.nan, index=result.index)
        veto_long_fade = np.zeros(n, dtype=bool)
        veto_short_fade = np.zeros(n, dtype=bool)

    up_family = (_TREND_UP_CLEAN, _TREND_UP_CHOPPY)
    down_family = (_TREND_DOWN_CLEAN, _TREND_DOWN_CHOPPY)
    mr_family = (_RANGING_QUIET, _RANGING_VOLATILE, _TREND_UP_CHOPPY, _TREND_DOWN_CHOPPY)

    pos = 0
    positions = np.zeros(n, dtype=int)
    for i in range(n):
        lab = labels[i]
        if lab == _WARMUP:
            pos = 0
            continue

        if pos == 1 and lab not in up_family:
            pos = 0
        elif pos == -1 and lab not in down_family:
            pos = 0
        elif pos == 2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] >= mr_exit_z) or lab == _TREND_DOWN_CLEAN
        ):
            pos = 0
        elif pos == -2 and (
            (not np.isnan(z_vals[i]) and z_vals[i] <= -mr_exit_z) or lab == _TREND_UP_CLEAN
        ):
            pos = 0

        if pos == 0:
            if lab == _TREND_UP_CLEAN and breakout_up[i]:
                pos = 1
            elif allow_short and lab == _TREND_DOWN_CLEAN and breakout_down[i]:
                pos = -1
            elif lab in mr_family:
                if mr_long_trig[i] and not veto_long_fade[i]:
                    pos = 2
                elif allow_short and mr_short_trig[i] and not veto_short_fade[i]:
                    pos = -2

        positions[i] = 1 if pos > 0 else (-1 if pos < 0 else 0)

    pos_series = pd.Series(positions, index=result.index)
    result["position"] = pos_series
    result["signal"] = pos_series.diff().fillna(0).clip(-1, 1).astype(int)
    result["ra_label"] = [_LABEL_NAMES[int(code)] for code in labels]
    result["ra_return_eff"] = return_eff
    result["ra_range_eff"] = range_eff
    result["ra_efficiency"] = efficiency
    result["ra_adx"] = adx
    result["ra_z"] = z
    result["ra_slow_eff"] = slow_eff
    return result
