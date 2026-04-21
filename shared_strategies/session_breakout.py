"""
Session Breakout — trade breakouts from session-specific highs/lows with volume
confirmation.

Differs from the ``breakout`` strategy (rolling N-bar high/low + ATR filter):

* Levels come from the most recently-completed *session* (e.g. Asian range,
  US open first hour) rather than a rolling window.
* A volume spike is mandatory for entry, filtering out low-conviction wicks.

Sessions are UTC-based. A bar is considered *after* the session once its hour
is past the session end — that guarantees the level is fixed before any
breakout can fire, avoiding look-ahead.
"""

import numpy as np
import pandas as pd


# UTC hour windows [start, end). ``end`` is exclusive.
SESSION_WINDOWS = {
    "asian":    (0, 8),     # Asian range
    "us_open":  (13, 14),   # first hour after NYSE open (13:30 UTC → 13:xx bars)
    "us_close": (20, 21),   # final hour before NYSE close
}


def session_breakout_core(
    df: pd.DataFrame,
    session: str = "asian",
    lookback: int = 1,
    volume_threshold: float = 1.5,
    vol_period: int = 20,
    atr_period: int = 14,
    atr_multiplier: float = 0.0,
) -> pd.DataFrame:
    """
    Generate session breakout signals with volume confirmation.

    Parameters
    ----------
    df : DataFrame with open/high/low/close/volume columns. A DatetimeIndex
         is required — without it, signals are all zero (session detection
         needs wall-clock hours).
    session : one of ``"asian"``, ``"us_open"``, ``"us_close"``.
    lookback : number of prior sessions to aggregate for the key level
               (rolling max of session highs / min of session lows).
    volume_threshold : multiplier vs ``vol_period`` SMA the breakout bar's
                       volume must exceed (default 1.5 = 150% of average).
    vol_period : window for the volume SMA.
    atr_period : window for the optional ATR filter.
    atr_multiplier : if > 0, the breakout bar's true range must exceed
                     ``atr * atr_multiplier`` (filters low-volatility chop).
                     Default 0 disables the filter.

    Returns
    -------
    DataFrame with added columns:
        signal        : 1 (bullish breakout), -1 (bearish breakout), 0 (hold)
        session_high  : prior lookback sessions' high (key level)
        session_low   : prior lookback sessions' low (key level)
        vol_sma       : rolling volume SMA
    """
    result = df.copy()
    result["signal"] = 0
    result["session_high"] = np.nan
    result["session_low"] = np.nan
    result["vol_sma"] = np.nan

    if not isinstance(result.index, pd.DatetimeIndex) or result.empty:
        return result

    start_hour, end_hour = SESSION_WINDOWS.get(session, SESSION_WINDOWS["asian"])
    hours = result.index.hour
    in_session = (hours >= start_hour) & (hours < end_hour)
    dates = result.index.normalize()

    # Per-day session high/low. Days with no in-session bars don't contribute
    # a row so a gappy series doesn't blank the levels.
    session_bars = result[in_session]
    sess_df = session_bars.groupby(session_bars.index.normalize()).agg(
        s_high=("high", "max"), s_low=("low", "min")
    )
    if sess_df.empty:
        return result

    sess_df["level_high"] = (
        sess_df["s_high"].rolling(window=lookback, min_periods=1).max().shift(1)
    )
    sess_df["level_low"] = (
        sess_df["s_low"].rolling(window=lookback, min_periods=1).min().shift(1)
    )

    # Forward-fill levels across days so a bar on day N uses the level built
    # from the last ``lookback`` sessions ending on day N-1.
    level_high_by_day = sess_df["level_high"]
    level_low_by_day = sess_df["level_low"]
    result["session_high"] = dates.to_series(index=result.index).map(level_high_by_day)
    result["session_low"] = dates.to_series(index=result.index).map(level_low_by_day)

    result["vol_sma"] = result["volume"].rolling(window=vol_period, min_periods=1).mean()
    high_volume = result["volume"] > result["vol_sma"] * volume_threshold

    if atr_multiplier > 0:
        tr = pd.concat(
            [
                result["high"] - result["low"],
                (result["high"] - result["close"].shift(1)).abs(),
                (result["low"] - result["close"].shift(1)).abs(),
            ],
            axis=1,
        ).max(axis=1)
        atr = tr.rolling(window=atr_period, min_periods=1).mean()
        atr_ok = tr > atr * atr_multiplier
    else:
        atr_ok = pd.Series(True, index=result.index)

    # Only fire after the session has closed for the day — an intra-session
    # "breakout" against its own forming level is meaningless.
    after_session = hours >= end_hour

    level_high = result["session_high"]
    level_low = result["session_low"]

    break_up = (
        (result["close"] > level_high) & high_volume & atr_ok & after_session
    )
    break_down = (
        (result["close"] < level_low) & high_volume & atr_ok & after_session
    )

    # First bar of a breakout only — stay-above doesn't repeat.
    result.loc[break_up & ~break_up.shift(1, fill_value=False), "signal"] = 1
    result.loc[break_down & ~break_down.shift(1, fill_value=False), "signal"] = -1

    return result
