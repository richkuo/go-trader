"""
MTF Confluence — higher-timeframe trend gate over lower-timeframe pullback
entries (#957).

The strategy audit (#956) showed the dominant failure mode across the registry
was high-churn entries fighting the higher-timeframe trend, with 4h runs
consistently beating their 1h twins. This strategy makes the multi-timeframe
gate first-class instead of the bolt-on ``htf_filter`` — and it derives the
higher-timeframe view from the SINGLE frame the framework already passes
(resampled in-process; no extra data fetch).

Rules (long; short is the mirror, gated by ``allow_short``)
-----------------------------------------------------------
1. HTF trend:  the native frame is resampled to ``htf_factor`` × the native
               bar (e.g. 1h → 4h). Trend is up when HTF EMA(fast) exceeds
               EMA(slow) by at least ``htf_sep_pct`` (epsilon wobble in a
               flat market is not a trend) AND EMA(fast) is rising
               bucket-over-bucket.
2. LTF entry:  within an HTF uptrend, a recent prior bar pulled back to
               (tagged) the native-frame EMA within ``pullback_window`` bars,
               and the current bar resumes the trend: close reclaims the EMA
               and breaks the prior bar's high.
3. Exit:       hysteresis — entries require the strict gate (separation +
               rising fast EMA) but a held position only exits when the HTF
               EMAs actually cross against it (fast ≤ slow for longs). The
               loose hold gate stops fee churn from bucket-to-bucket slope
               flicker; ``signal`` emits the transition (-1 closes a long,
               +1 closes a short).

Anti-look-ahead contract (CRITICAL)
-----------------------------------
An HTF bucket is only readable from the native bar at which the bucket has
fully CLOSED. Buckets are aligned to the epoch (window-start independent, the
same parity lesson as regime windows) and a bucket labeled ``L`` spanning
``[L, L+H)`` becomes visible at the native bar labeled ``L + H - native_period``
— the bar whose own close coincides with the bucket close. The in-progress
trailing bucket maps past the end of the frame and is never read. A signal at
bar N may therefore use bar N's own close, matching the engine contract that
the signal computed on bar N fills at bar N+1's open
(see backtest/tests/test_backtester_lookahead.py).
"""

import numpy as np
import pandas as pd

_HTF_AGG = {"open": "first", "high": "max", "low": "min", "close": "last"}


def _resample_htf(df: pd.DataFrame, htf_factor: int):
    """Resample the native frame into HTF buckets without look-ahead.

    Returns ``(htf, visible_at)`` where ``htf`` is the HTF OHLC frame (one row
    per non-empty bucket) and ``visible_at`` is, for each HTF row, the native
    index label at/after which that bucket is fully closed and may be read.

    Datetime-indexed frames use a wall-clock ``resample`` (epoch-aligned, so
    bucketing does not depend on where the fetch window starts). Frames
    without a usable DatetimeIndex fall back to positional bucketing
    (``htf_factor`` consecutive bars per bucket).
    """
    native_td = None
    if isinstance(df.index, pd.DatetimeIndex) and len(df) >= 3:
        diffs = df.index.to_series().diff().dropna()
        if len(diffs) > 0:
            cadence = diffs.mode()
            if len(cadence) > 0 and cadence.iloc[0] > pd.Timedelta(0):
                native_td = cadence.iloc[0]

    if native_td is not None:
        htf_td = native_td * htf_factor
        htf = (
            df[["open", "high", "low", "close"]]
            .resample(htf_td, label="left", closed="left", origin="epoch")
            .agg(_HTF_AGG)
            .dropna(subset=["close"])
        )
        # Bucket [L, L+H) closes when the native bar labeled L+H-p closes.
        visible_at = htf.index + htf_td - native_td
        return htf, visible_at

    # Positional fallback: buckets of htf_factor consecutive bars; a bucket is
    # readable from its last constituent bar onward. The trailing partial
    # bucket is dropped entirely (it has not closed).
    n = len(df)
    n_full = n // htf_factor
    if n_full == 0:
        empty = pd.DataFrame(columns=list(_HTF_AGG))
        return empty, df.index[:0]
    o = df["open"].to_numpy(dtype=float)[: n_full * htf_factor].reshape(n_full, htf_factor)
    h = df["high"].to_numpy(dtype=float)[: n_full * htf_factor].reshape(n_full, htf_factor)
    l = df["low"].to_numpy(dtype=float)[: n_full * htf_factor].reshape(n_full, htf_factor)
    c = df["close"].to_numpy(dtype=float)[: n_full * htf_factor].reshape(n_full, htf_factor)
    last_pos = np.arange(1, n_full + 1) * htf_factor - 1
    visible_at = df.index[last_pos]
    htf = pd.DataFrame(
        {
            "open": o[:, 0],
            "high": h.max(axis=1),
            "low": l.min(axis=1),
            "close": c[:, -1],
        },
        index=visible_at,
    )
    return htf, visible_at


def _project_to_native(values: pd.Series, visible_at, native_index) -> pd.Series:
    """Forward-fill an HTF series onto the native index, where each HTF value
    only becomes available at its bucket's ``visible_at`` native label."""
    proj = pd.Series(values.to_numpy(), index=visible_at)
    return proj.reindex(native_index, method="ffill")


def mtf_confluence_core(
    df: pd.DataFrame,
    htf_factor: int = 4,
    htf_ema_fast: int = 20,
    htf_ema_slow: int = 40,
    htf_sep_pct: float = 0.001,
    ltf_ema: int = 20,
    pullback_window: int = 6,
    pullback_touch_buffer_pct: float = 0.0,
    allow_short: bool = False,
) -> pd.DataFrame:
    """Generate MTF-confluence signals.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns
    htf_factor : native bars per HTF bucket (e.g. 4 turns 1h into 4h)
    htf_ema_fast / htf_ema_slow : EMAs (in HTF bars) defining the HTF trend
    htf_sep_pct : minimum relative EMA separation (|fast/slow - 1|) for the
        HTF trend to count; filters flat-market epsilon wobble
    ltf_ema : native-frame EMA acting as the pullback magnet / reclaim level
    pullback_window : native bars to look back for the EMA tag
    pullback_touch_buffer_pct : fraction the bar must pierce the LTF EMA by to
        count as a pullback tag (0 = a plain touch qualifies)
    allow_short : mirror entries in HTF downtrends (futures variant)

    Returns
    -------
    DataFrame with added columns:
        signal      : 1 / -1 / 0 (position-state transitions, clamped)
        position    : desired position state (1 long, -1 short, 0 flat)
        htf_trend   : visible HTF trend at each native bar (1 up, -1 down, 0)
        htf_ema_fast / htf_ema_slow : visible HTF EMAs (NaN during warmup)
        ltf_ema     : native-frame EMA
    """
    htf_factor = max(int(htf_factor), 1)
    result = df.copy()
    result["signal"] = 0
    result["position"] = 0
    result["htf_trend"] = 0
    result["htf_ema_fast"] = np.nan
    result["htf_ema_slow"] = np.nan
    result["ltf_ema"] = np.nan

    n = len(result)
    if n < htf_factor + 2:
        return result

    close = result["close"]
    high = result["high"]
    low = result["low"]

    # ── HTF view (closed buckets only) ──────────────────────────────────────
    htf, visible_at = _resample_htf(result, htf_factor)
    if len(htf) == 0:
        return result

    htf_close = htf["close"]
    ema_fast_htf = htf_close.ewm(span=htf_ema_fast, adjust=False).mean()
    ema_slow_htf = htf_close.ewm(span=htf_ema_slow, adjust=False).mean()
    # EMAs are defined from the first bucket but meaningless until the slow
    # span has seen enough buckets; hold the trend neutral during warmup.
    warm = np.arange(len(htf)) >= htf_ema_slow
    rising = ema_fast_htf > ema_fast_htf.shift(1)
    falling = ema_fast_htf < ema_fast_htf.shift(1)
    # Strict ENTRY gate: separated EMAs + rising fast EMA. Loose HOLD gate:
    # fast still on the right side of slow — slope flicker between buckets
    # must not churn an open position out and back in (fee drag).
    up_htf = (ema_fast_htf > ema_slow_htf * (1.0 + htf_sep_pct)) & rising & warm
    down_htf = (ema_fast_htf < ema_slow_htf * (1.0 - htf_sep_pct)) & falling & warm
    hold_up_htf = (ema_fast_htf > ema_slow_htf) & warm
    hold_down_htf = (ema_fast_htf < ema_slow_htf) & warm
    trend_htf = pd.Series(
        np.where(up_htf, 1, np.where(down_htf, -1, 0)), index=htf.index
    )

    trend = (
        _project_to_native(trend_htf, visible_at, result.index)
        .fillna(0)
        .astype(int)
    )
    hold_up = (
        _project_to_native(hold_up_htf, visible_at, result.index)
        .fillna(False)
        .astype(bool)
        .to_numpy()
    )
    hold_down = (
        _project_to_native(hold_down_htf, visible_at, result.index)
        .fillna(False)
        .astype(bool)
        .to_numpy()
    )
    result["htf_trend"] = trend
    result["htf_ema_fast"] = _project_to_native(ema_fast_htf, visible_at, result.index)
    result["htf_ema_slow"] = _project_to_native(ema_slow_htf, visible_at, result.index)

    # ── LTF pullback + resumption triggers ──────────────────────────────────
    ltf_ema_s = close.ewm(span=ltf_ema, adjust=False).mean()
    result["ltf_ema"] = ltf_ema_s

    # Positive buffer *tightens* the gate: the bar must pierce the EMA by the
    # fraction (low below for longs, high above for shorts); 0 = plain touch.
    long_touch = low < (ltf_ema_s * (1.0 - pullback_touch_buffer_pct))
    long_pullback = (
        long_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )
    short_touch = high > (ltf_ema_s * (1.0 + pullback_touch_buffer_pct))
    short_pullback = (
        short_touch.shift(1).rolling(window=pullback_window).max().fillna(0).astype(bool)
    )

    prev_high = high.shift(1)
    prev_low = low.shift(1)
    long_trig = ((close > ltf_ema_s) & (close > prev_high) & long_pullback).to_numpy()
    short_trig = ((close < ltf_ema_s) & (close < prev_low) & short_pullback).to_numpy()

    # ── Position state machine ───────────────────────────────────────────────
    # Enter only on a pullback-resumption trigger confirmed by the strict HTF
    # entry gate; exit (to flat) when the loose HTF hold gate fails (the EMAs
    # crossed against the position). Sequential because the exit condition
    # depends on the held side.
    trend_arr = trend.to_numpy()
    pos = np.zeros(n, dtype=int)
    state = 0
    for i in range(n):
        if state == 1 and not hold_up[i]:
            state = 0
        elif state == -1 and not hold_down[i]:
            state = 0
        if state == 0:
            if long_trig[i] and trend_arr[i] == 1:
                state = 1
            elif allow_short and short_trig[i] and trend_arr[i] == -1:
                state = -1
        pos[i] = state

    result["position"] = pos
    # A direct flip would yield diff == ±2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = np.clip(np.diff(pos, prepend=0), -1, 1)
    return result
