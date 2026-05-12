"""Tests for liquidity_sweeps.py — including a look-ahead regression covering #732.

Bug regressed: ``_find_swing_highs`` / ``_find_swing_lows`` use a centered
[i-lookback, i+lookback] window, so a swing at position p is only confirmable
after bar p+lookback has closed. The pre-fix main loop read
``swing_highs.iloc[i - 1]``, which consumes a swing classification that may
depend on bars > i. The fix lags the swing read by ``lookback`` bars:
``confirm_pos = i - lookback - 1``, so a swing at position p only becomes
``recent_swing_high`` at iteration ``p + lookback + 1`` — once the live
trading cycle has actually observed every bar in p's centered window.

A direct truncation-vs-full output-divergence test is structurally hard to
construct because the centered-window swing detector inherently rejects
classifications when the sweep candidate's wick contaminates the window. The
tests below instead validate the lag contract: signals never fire before the
swing's confirmation window has fully closed.
"""

import numpy as np
import pandas as pd

from liquidity_sweeps import _find_swing_lows, liquidity_sweep_core


def _make_ohlcv(highs, lows, closes, opens=None):
    n = len(closes)
    if opens is None:
        opens = closes
    return pd.DataFrame({
        "open":   opens,
        "high":   highs,
        "low":    lows,
        "close":  closes,
        "volume": [100.0] * n,
    })


def _monotone_uptrend(n: int, start: float = 100.0, slope: float = 0.5) -> tuple:
    """Strictly increasing baseline so the centered-window swing detector
    finds no local maxima/minima in flat stretches."""
    closes = start + slope * np.arange(n)
    highs = closes + 0.3
    lows = closes - 0.3
    return highs.astype(float), lows.astype(float), closes.astype(float)


class TestLookahead:
    """Regression for #732: swing reads must lag by ``lookback`` bars."""

    def test_sweep_never_fires_inside_confirmation_window(self):
        """For every detected swing at position p, no sweep signal may fire
        at any bar in ``[p+1, p+lookback]`` — the swing's centered window
        hasn't fully observed yet. The earliest legitimate consumption is
        bar ``p + lookback + 1`` (one bar after final confirmation, since the
        fix's read is ``swing_highs.iloc[i - lookback - 1]``).
        """
        lookback = 5
        n = 80

        # Construct a setup with one clean swing low at p=15 (its centered
        # window [10:21] sees no contamination), then a confirmed sweep at
        # bar p + lookback + 1 = 21.
        highs, lows, closes = _monotone_uptrend(n)
        p = 15
        lows[p] = 50.0
        closes[p] = 80.0
        # Sweep at the earliest legitimate consumption bar.
        q = p + lookback + 1
        lows[q] = 49.0
        closes[q] = 81.0

        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)

        # Identify every detected swing low and assert no signal landed
        # within its confirmation window.
        sl = _find_swing_lows(df["low"], lookback)
        swing_positions = np.where(sl.notna())[0].tolist()
        # At least the planted swing at p should be detected.
        assert p in swing_positions, "test setup failed to plant swing at p"

        for sp in swing_positions:
            # Inside window: positions sp..sp+lookback (no consumption yet).
            window_end = min(sp + lookback + 1, n)
            inside = out["signal"].iloc[sp + 1: window_end]
            assert (inside == 0).all(), (
                f"signal fired inside confirmation window of swing at {sp}: "
                f"non-zero at {[sp + 1 + i for i in np.where(inside != 0)[0]]}"
            )

    def test_signal_at_k_independent_of_bars_after_k(self):
        """Truncation invariant: regenerating the signal series from df[:K+1]
        must yield the same value at bar K as the full series. The fix's
        ``confirm_pos = i - lookback - 1`` only ever reads swing_highs at
        positions ≤ K-lookback-1, which are themselves classifications over
        windows fully contained in [0, K]. So bars > K cannot influence the
        decision at bar K.
        """
        lookback = 5
        n = 80
        highs, lows, closes = _monotone_uptrend(n)
        # Plant a swing far enough back that a sweep fires past its window.
        lows[20] = 60.0
        closes[20] = 70.0
        lows[40] = 59.0
        closes[40] = 80.0

        df = _make_ohlcv(highs, lows, closes)
        full = liquidity_sweep_core(df, swing_lookback=lookback)

        # For every bar K where a signal fires in the full series, regenerate
        # from the prefix and assert the value at K matches.
        signal_bars = list(np.where(full["signal"].values != 0)[0])
        for k in signal_bars:
            partial_df = df.iloc[: k + 1]
            partial = liquidity_sweep_core(partial_df, swing_lookback=lookback)
            assert partial["signal"].iloc[k] == full["signal"].iloc[k], (
                f"signal at bar {k} differs after truncation: "
                f"full={full['signal'].iloc[k]} truncated={partial['signal'].iloc[k]}"
            )


class TestBasic:
    def test_bullish_sweep_after_confirmation(self):
        """A swing low confirmed long before the sweep should fire a buy."""
        lookback = 3
        n = 40
        highs, lows, closes = _monotone_uptrend(n)
        lows[10] = 50.0
        closes[10] = 80.0
        lows[25] = 49.0
        closes[25] = 81.0

        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)
        assert (out["signal"].iloc[25:] == 1).any()

    def test_no_signal_when_close_doesnt_recover(self):
        lookback = 3
        n = 40
        highs, lows, closes = _monotone_uptrend(n)
        lows[10] = 50.0; closes[10] = 80.0
        lows[25] = 49.0; closes[25] = 48.0  # close stays below 50
        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)
        assert (out["signal"] == 1).sum() == 0

    def test_short_data_returns_zeros(self):
        df = _make_ohlcv([100.5] * 5, [99.5] * 5, [100] * 5)
        out = liquidity_sweep_core(df, swing_lookback=5)
        assert (out["signal"] == 0).all()
