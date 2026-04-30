"""
AMD+IFVG — ICT Accumulation-Manipulation-Distribution with Implied Fair Value Gap.

Session-aware price action strategy on 15m candles:
1. Accumulation: Identify Asian session range (high/low)
2. Manipulation: Detect London open sweep beyond Asian range (stop hunt)
3. IFVG Detection: Find 3-candle imbalance gap created during manipulation
4. Entry: Price retraces into the IFVG -> fire signal in direction of reversal

Signal: 1 = BUY, -1 = SELL, 0 = FLAT
"""

import numpy as np
import pandas as pd


def amd_ifvg_core(
    df: pd.DataFrame,
    asian_start_hour: int = 0,
    asian_end_hour: int = 8,
    london_start_hour: int = 8,
    london_end_hour: int = 12,
    min_ifvg_pct: float = 0.05,
    sweep_threshold_pct: float = 0.01,
) -> pd.DataFrame:
    """
    AMD+IFVG strategy core logic.

    Parameters:
        df: OHLCV DataFrame with UTC datetime index
        asian_start_hour: UTC hour Asian session begins (inclusive)
        asian_end_hour: UTC hour Asian session ends (exclusive)
        london_start_hour: UTC hour London kill zone begins (inclusive)
        london_end_hour: UTC hour London kill zone ends (exclusive)
        min_ifvg_pct: Minimum IFVG gap as percentage of price (0.05 = 0.05%)
        sweep_threshold_pct: Penetration beyond Asian range as fraction of range size

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

    hours = result.index.hour
    dates = result.index.date

    # Process each trading day
    for day in pd.unique(dates):
        day_mask = dates == day

        # --- Phase 1: Accumulation — Asian session range ---
        asian_mask = day_mask & (hours >= asian_start_hour) & (hours < asian_end_hour)
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
        london_mask = day_mask & (hours >= london_start_hour) & (hours < london_end_hour)
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

        # --- Phase 3: IFVG Detection — 3-candle imbalance gap ---
        # Scan candles from the sweep onward for fair value gaps
        post_sweep_mask = day_mask & (result.index >= sweep_idx)
        post_sweep = result.loc[post_sweep_mask]

        if len(post_sweep) < 3:
            continue

        best_ifvg = None
        best_ifvg_idx = None
        latest_close = post_sweep["close"].iloc[-1]

        ps_indices = post_sweep.index.tolist()
        for i in range(2, len(ps_indices)):
            c0 = post_sweep.loc[ps_indices[i - 2]]  # candle before displacement
            c2 = post_sweep.loc[ps_indices[i]]       # candle after displacement

            if bias == -1:
                # Bullish IFVG: gap up (c0 high < c2 low)
                if c0["high"] < c2["low"]:
                    gap_low = c0["high"]
                    gap_high = c2["low"]
                    gap_size = gap_high - gap_low
                    mid_price = (gap_high + gap_low) / 2
                    if mid_price > 0 and (gap_size / mid_price * 100) >= min_ifvg_pct:
                        dist = abs(latest_close - (gap_high + gap_low) / 2)
                        if best_ifvg is None or dist < best_ifvg[2]:
                            best_ifvg = (gap_low, gap_high, dist)
                            best_ifvg_idx = ps_indices[i]
            else:
                # Bearish IFVG: gap down (c0 low > c2 high)
                if c0["low"] > c2["high"]:
                    gap_high = c0["low"]
                    gap_low = c2["high"]
                    gap_size = gap_high - gap_low
                    mid_price = (gap_high + gap_low) / 2
                    if mid_price > 0 and (gap_size / mid_price * 100) >= min_ifvg_pct:
                        dist = abs(latest_close - (gap_high + gap_low) / 2)
                        if best_ifvg is None or dist < best_ifvg[2]:
                            best_ifvg = (gap_low, gap_high, dist)
                            best_ifvg_idx = ps_indices[i]

        if best_ifvg is None:
            continue

        ifvg_low, ifvg_high = best_ifvg[0], best_ifvg[1]
        result.loc[day_mask, "ifvg_high"] = ifvg_high
        result.loc[day_mask, "ifvg_low"] = ifvg_low

        # --- Phase 4: Entry — Retracement into IFVG ---
        # Look for price entering the IFVG zone after it forms
        entry_mask = day_mask & (result.index > best_ifvg_idx)
        entry_candles = result.loc[entry_mask]

        signal_fired = False
        for idx in entry_candles.index:
            if signal_fired:
                break
            row = entry_candles.loc[idx]

            if bias == -1:
                # Bullish entry: price dips into bullish IFVG
                if row["low"] <= ifvg_high and row["close"] >= ifvg_low:
                    result.loc[idx, "signal"] = 1
                    signal_fired = True
            else:
                # Bearish entry: price rallies into bearish IFVG
                if row["high"] >= ifvg_low and row["close"] <= ifvg_high:
                    result.loc[idx, "signal"] = -1
                    signal_fired = True

    return result
