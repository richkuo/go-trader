"""Tests for donchian_breakout.py — Donchian Channel Breakout strategy."""

import numpy as np
import pandas as pd
import pytest

from donchian_breakout import donchian_breakout_core


# ─── Helpers ────────────────────────────────

def make_ohlcv(closes, volume=None, noise=0.5):
    """Build an OHLCV DataFrame from a close price array."""
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    highs = closes + noise
    lows = closes - noise
    opens = closes - noise * 0.3
    return pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "volume": np.array(volume, dtype=float),
    })


# ─── Tests ──────────────────────────────────

def test_breakout_above_channel_generates_buy():
    """Range-bound data then breakout upward should produce a buy signal."""
    prices = [100] * 30 + list(np.linspace(100, 120, 20))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert (result["signal"] == 1).any(), "Expected at least one buy signal on upward breakout"


def test_breakdown_below_channel_generates_sell():
    """Range-bound data then breakdown downward should produce a sell signal."""
    prices = [100] * 30 + list(np.linspace(100, 80, 20))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert (result["signal"] == -1).any(), "Expected at least one sell signal on downward breakdown"


def test_flat_market_no_signals():
    """Flat data with no noise should produce no signals."""
    prices = [100.0] * 50
    df = make_ohlcv(prices, noise=0)
    result = donchian_breakout_core(df)
    assert (result["signal"] == 0).all(), "Flat market should produce no breakout signals"


def test_short_data_no_crash():
    """Very short data should return a signal column without crashing."""
    df = make_ohlcv([100, 101, 102, 103, 104])
    result = donchian_breakout_core(df)
    assert "signal" in result.columns
    assert len(result) == 5
    assert (result["signal"] == 0).all(), "Short data should produce no signals"


def test_signal_values_valid():
    """All signal values should be in {-1, 0, 1}."""
    prices = list(np.linspace(80, 120, 30)) + list(np.linspace(120, 80, 30))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})


def test_no_lookahead_bias():
    """Channel at bar i must be based on bars before i, not including i.

    With 25 bars of flat data at 100 followed by a breakout candle at 110,
    the breakout candle (index 25) should fire a buy signal because it breaks
    above the channel computed from the prior 20 bars (all at 100).
    """
    prices = [100] * 25 + [110]
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    # The channel upper at index 25 is max(high[5:25]) shifted by 1 = 100 + noise = 100.5
    # Close at index 25 is 110 > 100.5, so signal should be 1
    assert result["signal"].iloc[25] == 1, (
        "Breakout candle should fire buy signal (channel based on prior bars)"
    )
    # Bars in the flat region should NOT have signals
    assert (result["signal"].iloc[:25] == 0).all(), (
        "No signals expected in the flat region before breakout"
    )


def test_channels_exposed():
    """Output should include donchian_upper, donchian_lower, and exit channel columns."""
    df = make_ohlcv([100] * 30)
    result = donchian_breakout_core(df)
    assert "donchian_upper" in result.columns
    assert "donchian_lower" in result.columns
    assert "donchian_exit_upper" in result.columns
    assert "donchian_exit_lower" in result.columns
    assert (result["signal"] == 0).all(), "Flat data should produce no breakout signals"
