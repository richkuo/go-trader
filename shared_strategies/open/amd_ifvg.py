"""
AMD+IFVG — ICT Accumulation-Manipulation-Distribution with Implied Fair Value Gap.

Session-aware price-action strategy on 15m candles:
1. Accumulation: Identify the Asian-session range (high/low)
2. Manipulation: Detect the London-open sweep beyond the Asian range (stop hunt)
3. IFVG Detection: Find the 3-candle imbalance gap created during manipulation
4. Entry: Price retraces into the IFVG -> fire signal in direction of reversal

Session windows are civil-time (ICT) windows anchored to ``session_tz``
(default America/New_York, the canonical ICT reference) and are therefore
DST-aware: the same civil session maps to a UTC hour that shifts by one hour
across daylight-saving transitions. The defaults are the canonical ICT
killzones — Asian range 20:00–00:00 ET (accumulation, prior evening) and the
London open killzone 02:00–05:00 ET (manipulation).

Because the Asian range forms the evening *before* the London/NY session it
manipulates, the logical "session day" is anchored at the Asian open
(``asian_start_hour`` in ``session_tz``) so the accumulation, manipulation and
distribution phases of one ICT day share a grouping key even though they
straddle civil midnight.

Signal: 1 = BUY, -1 = SELL, 0 = FLAT
"""

import numpy as np
import pandas as pd


def _session_local(index: pd.DatetimeIndex, session_tz: str) -> pd.DatetimeIndex:
    """Return ``index`` as naive wall-clock time in ``session_tz``.

    Bars are stored in UTC — tz-aware UTC from the live cache, or tz-naive UTC
    from the backtest loader. Either way a naive index is interpreted as UTC,
    converted to the civil session timezone, then stripped of tz info so
    ``.hour`` and date grouping read civil wall-clock — the DST-aware behaviour
    the ICT session windows require. ``tz_convert`` from UTC is unambiguous
    across DST: every UTC instant maps to exactly one civil wall-clock time.
    """
    if index.tz is None:
        local = index.tz_localize("UTC").tz_convert(session_tz)
    else:
        local = index.tz_convert(session_tz)
    return local.tz_localize(None)


def _hours_in_window(hours: np.ndarray, start_hour: int, end_hour: int) -> np.ndarray:
    """Boolean mask for civil ``hours`` in the half-open window
    ``[start_hour, end_hour)``, supporting windows that wrap past midnight.

    ``end_hour == 0`` denotes midnight as the end of the day (24), so a window
    like 20:00–00:00 selects hours {20, 21, 22, 23} rather than the empty set.
    """
    end_eff = 24 if end_hour == 0 else end_hour
    start_n = start_hour % 24
    if start_n < end_eff:
        return (hours >= start_n) & (hours < end_eff)
    # wraps past midnight (e.g. 22 -> 2 selects {22, 23, 0, 1})
    return (hours >= start_n) | (hours < (end_eff % 24))


def amd_ifvg_core(
    df: pd.DataFrame,
    asian_start_hour: int = 20,
    asian_end_hour: int = 0,
    london_start_hour: int = 2,
    london_end_hour: int = 5,
    min_ifvg_pct: float = 0.05,
    sweep_threshold_pct: float = 0.01,
    session_tz: str = "America/New_York",
) -> pd.DataFrame:
    """
    AMD+IFVG strategy core logic.

    Parameters:
        df: OHLCV DataFrame with a UTC datetime index (tz-aware or tz-naive UTC).
        asian_start_hour: civil hour (in ``session_tz``) the Asian session begins
            (inclusive). Also anchors the logical session day.
        asian_end_hour: civil hour the Asian session ends (exclusive; 0 = midnight).
        london_start_hour: civil hour the London kill zone begins (inclusive).
        london_end_hour: civil hour the London kill zone ends (exclusive).
        min_ifvg_pct: Minimum IFVG gap as percentage of price (0.05 = 0.05%).
        sweep_threshold_pct: Penetration beyond Asian range as fraction of range size.
        session_tz: IANA timezone the session windows are anchored to. Default
            ``America/New_York`` (canonical ICT). DST-aware. Pass ``"UTC"`` to
            recover fixed UTC-hour windows (legacy behaviour).

    Returns:
        DataFrame with signal column and indicator columns added.
    """
    result = df.copy()
    n = len(result)

    # Initialize output columns
    result["signal"] = 0
    result["asian_high"] = np.nan
    result["asian_low"] = np.nan
    result["ifvg_high"] = np.nan
    result["ifvg_low"] = np.nan
    result["sweep_dir"] = 0  # 1=above, -1=below, 0=none

    if n < 3:
        return result

    # Civil-time (DST-aware) session derivation. The session day is anchored at
    # the Asian open so the Asian range (prior evening) groups with the London
    # manipulation and NY distribution that follow it across civil midnight.
    local = _session_local(result.index, session_tz)
    hours = np.asarray(local.hour)
    session_day = np.asarray(
        (local - pd.Timedelta(hours=asian_start_hour % 24)).floor("D")
    )

    asian_hour_mask = _hours_in_window(hours, asian_start_hour, asian_end_hour)
    london_hour_mask = _hours_in_window(hours, london_start_hour, london_end_hour)

    # Process each trading day
    for day in pd.unique(session_day):
        day_mask = session_day == day

        # --- Phase 1: Accumulation — Asian session range ---
        asian_mask = day_mask & asian_hour_mask
        asian_candles = result.loc[asian_mask]

        if len(asian_candles) < 2:
            continue

        asian_high = asian_candles["high"].max()
        asian_low = asian_candles["low"].min()
        asian_range = asian_high - asian_low

        if asian_range <= 0:
            continue

        # Write Asian range to all candles for the day
        result.loc[day_mask, "asian_high"] = asian_high
        result.loc[day_mask, "asian_low"] = asian_low

        # --- Phase 2: Manipulation — London session sweep detection ---
        london_mask = day_mask & london_hour_mask
        london_candles = result.loc[london_mask]

        if len(london_candles) < 3:
            continue

        sweep_threshold = asian_range * sweep_threshold_pct

        # Find first sweep in each direction
        swept_below_idx = None
        swept_above_idx = None

        for idx in london_candles.index:
            row = london_candles.loc[idx]
            if swept_below_idx is None and row["low"] < (asian_low - sweep_threshold):
                swept_below_idx = idx
            if swept_above_idx is None and row["high"] > (asian_high + sweep_threshold):
                swept_above_idx = idx

        # Determine bias from first sweep
        if swept_below_idx is not None and swept_above_idx is not None:
            # Both swept — use whichever happened first
            if swept_below_idx <= swept_above_idx:
                bias = -1  # swept below first → bullish reversal
            else:
                bias = 1  # swept above first → bearish reversal
        elif swept_below_idx is not None:
            bias = -1  # swept below → bullish reversal
        elif swept_above_idx is not None:
            bias = 1  # swept above → bearish reversal
        else:
            continue  # no sweep — no setup

        result.loc[london_mask, "sweep_dir"] = bias
        sweep_idx = swept_below_idx if bias == -1 else swept_above_idx

        # --- Phase 3+4: IFVG Detection + Entry, processed bar-by-bar ---
        # An IFVG forms across 3 consecutive candles; it is only observable at
        # its completion bar (c2). For each candidate entry bar K, we select
        # the IFVG nearest to bar K's close using ONLY IFVGs that have already
        # completed at bars strictly before K — no day-final-close peek.
        post_sweep_mask = day_mask & (result.index >= sweep_idx)
        post_sweep = result.loc[post_sweep_mask]

        if len(post_sweep) < 3:
            continue

        ps_indices = post_sweep.index.tolist()

        # Pre-compute every IFVG candidate, indexed by its completion position
        # in ps_indices. Candidate is (gap_low, gap_high, completion_position).
        ifvg_candidates = []
        for i in range(2, len(ps_indices)):
            c0 = post_sweep.loc[ps_indices[i - 2]]  # candle before displacement
            c2 = post_sweep.loc[ps_indices[i]]      # candle after displacement

            if bias == -1:
                if c0["high"] >= c2["low"]:
                    continue
                gap_low, gap_high = c0["high"], c2["low"]
            else:
                if c0["low"] <= c2["high"]:
                    continue
                gap_low, gap_high = c2["high"], c0["low"]

            gap_size = gap_high - gap_low
            mid_price = (gap_high + gap_low) / 2
            if mid_price <= 0:
                continue
            if (gap_size / mid_price * 100) < min_ifvg_pct:
                continue
            ifvg_candidates.append((gap_low, gap_high, i))

        if not ifvg_candidates:
            continue

        signal_fired = False
        chosen_ifvg = None
        chosen_entry_idx = None

        # Walk entry bars in time order. At each bar K, only IFVGs completed
        # strictly before K are visible; pick the one whose midpoint is closest
        # to THIS bar's close.
        for k in range(1, len(ps_indices)):
            available = [c for c in ifvg_candidates if c[2] < k]
            if not available:
                continue

            bar_idx = ps_indices[k]
            bar_close = result.loc[bar_idx, "close"]
            bar_low = result.loc[bar_idx, "low"]
            bar_high = result.loc[bar_idx, "high"]

            best = min(
                available,
                key=lambda c: abs(bar_close - (c[0] + c[1]) / 2),
            )
            ifvg_low, ifvg_high = best[0], best[1]

            if bias == -1:
                # Bullish entry: price dips into bullish IFVG
                if bar_low <= ifvg_high and bar_close >= ifvg_low:
                    result.loc[bar_idx, "signal"] = 1
                    signal_fired = True
            else:
                # Bearish entry: price rallies into bearish IFVG
                if bar_high >= ifvg_low and bar_close <= ifvg_high:
                    result.loc[bar_idx, "signal"] = -1
                    signal_fired = True

            if signal_fired:
                chosen_ifvg = (ifvg_low, ifvg_high)
                chosen_entry_idx = bar_idx
                break

        # Stamp viz columns only from the entry bar onward so historical bars
        # never carry an IFVG level chosen with future data.
        if chosen_ifvg is not None and chosen_entry_idx is not None:
            viz_mask = day_mask & (result.index >= chosen_entry_idx)
            result.loc[viz_mask, "ifvg_low"] = chosen_ifvg[0]
            result.loc[viz_mask, "ifvg_high"] = chosen_ifvg[1]

    return result
