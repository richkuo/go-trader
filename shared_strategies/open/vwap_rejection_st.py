"""
VWAP Rejection Short — intraday short on weak rallies into VWAP / EMA20 / EMA50
that fail to reclaim and roll over inside a bearish higher-timeframe regime.

Rules
-----
1. Bearish regime: EMA(mid) < EMA(long) (e.g. EMA50 < EMA200) on the working frame.
2. Rally into resistance: a recent bar's high *exceeded* session VWAP, EMA(short),
   or EMA(mid) by ``rally_touch_buffer_pct`` — separates a real rally into
   resistance from a noisy wick tagging the level.
3. RSI cannot reclaim 50: the trigger bar's RSI must sit at or below
   ``rsi_max_reclaim`` (default 50) — bearish momentum still in control.
4. Trigger: a rejection candle — close < open AND close back below *every*
   rally-magnet level (VWAP, EMA(short), and EMA(mid)) — confirms loss of
   reclaim. The trigger set must mirror the rally set so a rally that
   pierced only EMA(mid) (e.g. early-session bear-flag where VWAP/EMA20
   already sit lower) cannot fire a short whose "rejection" never
   actually closed back below the level that was pierced.

Emits ``signal = -1`` on the trigger bar; otherwise 0.

Notes
-----
* VWAP is session-anchored using the bar's calendar date. Requires a
  ``DatetimeIndex``; if absent the index is coerced via ``pd.to_datetime``.
* Volume is needed for VWAP — flat-volume series still work but the VWAP
  collapses to a typical-price moving anchor.
"""

import numpy as np
import pandas as pd

from indicators_core import wilder_rsi


def _session_vwap(df: pd.DataFrame) -> pd.Series:
    """Session-anchored VWAP keyed by calendar date.

    Cumulative within each day so it resets at the session boundary, matching
    how intraday traders read the level.
    """
    if isinstance(df.index, pd.DatetimeIndex):
        day = df.index.date
    else:
        day = pd.to_datetime(df.index).date
    typical = (df["high"] + df["low"] + df["close"]) / 3.0
    tp_vol = typical * df["volume"]
    grouped_tp = tp_vol.groupby(day).cumsum()
    grouped_vol = df["volume"].groupby(day).cumsum()
    # Avoid div-by-zero on zero-volume bars; fall back to typical price.
    vwap = grouped_tp / grouped_vol.replace(0, np.nan)
    return vwap.fillna(typical)


def vwap_rejection_st_core(
    df: pd.DataFrame,
    ema_short: int = 20,
    ema_mid: int = 50,
    ema_long: int = 200,
    rsi_period: int = 14,
    rsi_max_reclaim: float = 50.0,
    rally_window: int = 5,
    rally_touch_buffer_pct: float = 0.001,
) -> pd.DataFrame:
    """Generate short signals on VWAP/EMA rejections inside a bearish regime.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    ema_short / ema_mid / ema_long : EMAs for the rally magnet + regime filter
    rsi_period : Wilder RSI lookback
    rsi_max_reclaim : trigger bar RSI must be ≤ this (default 50 — momentum
        still bearish; a clean reclaim above 50 invalidates the setup)
    rally_window : bars to look back for the rally touch
    rally_touch_buffer_pct : fraction by which the bar's high must *exceed*
        VWAP / EMA(short) / EMA(mid) to count as a rally touch — guards against
        wicks merely tagging the level

    Returns
    -------
    DataFrame with added columns:
        signal      : -1 (short), 0 (no entry)
        ema_short   : EMA(close, ema_short)
        ema_mid     : EMA(close, ema_mid)
        ema_long    : EMA(close, ema_long)
        vwap        : session-anchored VWAP
        rsi         : Wilder RSI (NaN during warmup)
    """
    result = df.copy()
    result["signal"] = 0

    n = len(result)
    min_len = max(ema_long, rsi_period) + rally_window + 2
    if n < min_len:
        result["ema_short"] = result["close"].ewm(span=ema_short, adjust=False).mean()
        result["ema_mid"] = result["close"].ewm(span=ema_mid, adjust=False).mean()
        result["ema_long"] = result["close"].ewm(span=ema_long, adjust=False).mean()
        result["vwap"] = result["close"] if n == 0 else _session_vwap(result)
        result["rsi"] = np.nan
        return result

    close = result["close"]
    open_ = result["open"]
    high = result["high"]

    result["ema_short"] = close.ewm(span=ema_short, adjust=False).mean()
    result["ema_mid"] = close.ewm(span=ema_mid, adjust=False).mean()
    result["ema_long"] = close.ewm(span=ema_long, adjust=False).mean()
    result["vwap"] = _session_vwap(result)

    result["rsi"] = wilder_rsi(close, rsi_period)

    bearish_regime = result["ema_mid"] < result["ema_long"]

    # Rally into resistance: high must *exceed* VWAP / EMA(short) / EMA(mid) by
    # the buffer. Wicks that only tag the level don't count. The buffer is
    # applied asymmetrically — overshoot side only — by design: we want a
    # decisive pierce on the way up, but the rejection close below a level
    # is meaningful even without buffer (a clean close-through is its own
    # confirmation).
    touch_mult = 1.0 + rally_touch_buffer_pct
    rally_touch = (
        (high > result["vwap"] * touch_mult)
        | (high > result["ema_short"] * touch_mult)
        | (high > result["ema_mid"] * touch_mult)
    )
    # .shift(1) so the rally must come from a *prior* bar — we never let the
    # trigger bar itself satisfy the touch.
    rally_recent = (
        rally_touch.shift(1).rolling(window=rally_window).max().fillna(0).astype(bool)
    )

    # RSI cannot reclaim 50 on the trigger bar — bearish momentum gate.
    rsi_capped = result["rsi"] <= rsi_max_reclaim

    # Rejection candle: red bar (close < open) sitting back below VWAP AND
    # EMA(short) AND EMA(mid). All three rally magnets must be lost — the
    # trigger set mirrors the rally set so whichever level was pierced gets
    # rejected. A close below VWAP but still above EMA(mid) is ambiguous.
    red_bar = close < open_
    below_vwap = close < result["vwap"]
    below_ema_short = close < result["ema_short"]
    below_ema_mid = close < result["ema_mid"]
    trigger = red_bar & below_vwap & below_ema_short & below_ema_mid

    short_mask = bearish_regime & rally_recent & rsi_capped & trigger
    result.loc[short_mask, "signal"] = -1
    return result
